/*
Image Action

Creates a raw filesystem image without a partition table. This is simpler
than the image-partition action and suitable for embedded targets that boot
directly from a filesystem image (e.g. rootfs on a raw eMMC partition,
network boot images, container base images).

	# Yaml syntax:
	- action: image
	  imagename: rootfs.img
	  imagesize: 2GB
	  fs: ext4
	  fslabel: rootfs
	  fsuuid: "..."
	  features: [...]
	  extendedoptions: []
	  fsck: true
	  mountpoint: /
	  options: []

Mandatory properties:

- imagename -- the name of the image file, relative to the artifact directory.

- imagesize -- image size in human-readable form, e.g. 512MB, 2GB.

- fs -- filesystem type. Supports the same types as the image-partition action,
except 'none'.

Optional properties:

- fslabel -- filesystem label. Defaults to the imagename without its extension.

- fsuuid -- fixed filesystem UUID. For ext2/ext3/ext4/btrfs/xfs this must be a
standard UUID string. For fat variants it must be an 8-character hexadecimal string.

- features -- list of additional filesystem features to enable.

- extendedoptions -- list of extended filesystem options (passed to mkfs via -E).

- fsck -- if false, set fs_passno to 0 in fstab, disabling boot-time filesystem
checks. Default: true.

- mountpoint -- mount path recorded in /etc/fstab. Default: "/".

- options -- list of additional fstab mount options.
*/
package actions

import (
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/docker/go-units"
	"github.com/freddierice/go-losetup/v2"
	"github.com/go-debos/debos"
	"github.com/go-debos/fakemachine"
	"github.com/google/uuid"
)

type ImageAction struct {
	debos.BaseAction `yaml:",inline"`
	ImageName        string
	ImageSize        string
	FS               string
	FSLabel          string   `yaml:"fslabel"`
	FSUUID           string   `yaml:"fsuuid"`
	Features         []string
	ExtendedOptions  []string `yaml:"extendedoptions"`
	Fsck             bool
	Mountpoint       string
	Options          []string
	// internal
	size      int64
	loopDev   losetup.Device
	usingLoop bool
}

func NewImageAction() *ImageAction {
	return &ImageAction{Fsck: true, Mountpoint: "/"}
}

func (i *ImageAction) Verify(_ *debos.Context) error {
	if i.ImageName == "" {
		return fmt.Errorf("imagename is required for image action")
	}
	if i.ImageSize == "" {
		return fmt.Errorf("imagesize is required for image action")
	}
	if i.FS == "" {
		return fmt.Errorf("fs is required for image action")
	}
	if i.FS == "none" {
		return fmt.Errorf("fs 'none' is not valid for image action")
	}

	if len(i.FSUUID) > 0 {
		switch i.FS {
		case "btrfs", "ext2", "ext3", "ext4", "xfs":
			_, err := uuid.Parse(i.FSUUID)
			if err != nil {
				return fmt.Errorf("incorrect UUID %s", i.FSUUID)
			}
		case "fat", "fat12", "fat16", "fat32", "msdos", "vfat":
			_, err := hex.DecodeString(i.FSUUID)
			if err != nil || len(i.FSUUID) != 8 {
				return fmt.Errorf("incorrect UUID %s, should be 32-bit hexadecimal number", i.FSUUID)
			}
		default:
			return fmt.Errorf("setting the UUID is not supported for filesystem %s", i.FS)
		}
	}

	if i.FSLabel == "" {
		ext := path.Ext(i.ImageName)
		if ext != "" {
			i.FSLabel = strings.TrimSuffix(i.ImageName, ext)
		} else {
			i.FSLabel = i.ImageName
		}
	}

	if i.Mountpoint == "" {
		i.Mountpoint = "/"
	}

	var getSizeValueFunc func(size string) (int64, error)
	if regexp.MustCompile(`^[0-9.]+[kmgtp]ib+$`).MatchString(strings.ToLower(i.ImageSize)) {
		getSizeValueFunc = units.RAMInBytes
	} else {
		getSizeValueFunc = units.FromHumanSize
	}

	size, err := getSizeValueFunc(i.ImageSize)
	if err != nil {
		return fmt.Errorf("failed to parse image size: %s", i.ImageSize)
	}
	i.size = size

	return nil
}

func (i ImageAction) PreMachine(context *debos.Context, m *fakemachine.Machine, args *[]string) error {
	imagePath := path.Join(context.Artifactdir, i.ImageName)
	image, err := m.CreateImage(imagePath, i.size)
	if err != nil {
		return err
	}
	context.Image = image
	*args = append(*args, "--internal-image", image)
	return nil
}

func (i *ImageAction) PreNoMachine(context *debos.Context) error {
	imagePath := path.Join(context.Artifactdir, i.ImageName)
	img, err := os.OpenFile(imagePath, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return fmt.Errorf("couldn't open image file: %w", err)
	}
	err = img.Truncate(i.size)
	if err != nil {
		return fmt.Errorf("couldn't resize image file: %w", err)
	}
	img.Close()

	retries := 60
	for t := 1; t <= retries; t++ {
		i.loopDev, err = losetup.Attach(imagePath, 0, false)
		if err == nil {
			break
		}
		log.Printf("Setup loop device: try %d/%d failed: %v", t, retries, err)
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("failed to setup loop device: %w", err)
	}

	context.Image = i.loopDev.Path()
	i.usingLoop = true
	return nil
}

func (i ImageAction) Run(context *debos.Context) error {
	p := Partition{
		FS:              i.FS,
		FSLabel:         i.FSLabel,
		FSUUID:          i.FSUUID,
		Features:        i.Features,
		ExtendedOptions: i.ExtendedOptions,
		Fsck:            i.Fsck,
	}

	lock, err := lockImage(context)
	if err != nil {
		return err
	}
	err = formatFilesystem("Formatting image", context.Image, &p)
	lock.unlock()
	if err != nil {
		return err
	}

	context.ImageMntDir = path.Join(context.Scratchdir, "mnt")
	if err := os.MkdirAll(context.ImageMntDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount directory: %w", err)
	}

	fsType := p.FS
	switch p.FS {
	case "fat", "fat12", "fat16", "fat32", "msdos":
		fsType = "vfat"
	}
	if err := syscall.Mount(context.Image, context.ImageMntDir, fsType, 0, ""); err != nil {
		return fmt.Errorf("image mount failed: %w", err)
	}

	options := []string{"defaults"}
	options = append(options, i.Options...)

	fsPassno := 0
	if i.Fsck {
		if i.Mountpoint == "/" {
			fsPassno = 1
		} else {
			fsPassno = 2
		}
	}

	fstabFSType := p.FS
	switch p.FS {
	case "fat", "fat12", "fat16", "fat32", "msdos":
		fstabFSType = "vfat"
	}

	context.ImageFSTab.WriteString(fmt.Sprintf("UUID=%s\t%s\t%s\t%s\t0\t%d\n",
		p.FSUUID, i.Mountpoint, fstabFSType,
		strings.Join(options, ","), fsPassno))

	if i.Mountpoint == "/" {
		context.ImageKernelRoot = fmt.Sprintf("root=UUID=%s", p.FSUUID)
	}

	if err := (debos.Command{}.Run("udevadm", "udevadm", "trigger", "--settle", context.Image)); err != nil {
		log.Printf("Failed to trigger device nodes: %v", err)
	}

	return nil
}

func (i ImageAction) Cleanup(context *debos.Context) error {
	if context.ImageMntDir != "" {
		err := syscall.Unmount(context.ImageMntDir, 0)
		if err != nil {
			log.Printf("Warning: Failed to unmount %s: %s", context.ImageMntDir, err)
			log.Printf("Unmount failure can cause images being incomplete!")
			return err
		}
	}

	if i.usingLoop {
		err := i.loopDev.Detach()
		if err != nil {
			log.Printf("WARNING: Failed to detach loop device: %s", err)
			return err
		}

		for t := 0; t < 60; t++ {
			err = i.loopDev.Remove()
			if err == nil {
				break
			}
			time.Sleep(time.Second)
		}

		if err != nil {
			log.Printf("WARNING: Failed to remove loop device: %s", err)
			return err
		}
	}

	return nil
}

func (i ImageAction) PostMachineCleanup(context *debos.Context) error {
	image := path.Join(context.Artifactdir, i.ImageName)
	if context.State != debos.Success {
		if _, err := os.Stat(image); !os.IsNotExist(err) {
			if err = os.Remove(image); err != nil {
				return err
			}
		}
	}
	return nil
}
