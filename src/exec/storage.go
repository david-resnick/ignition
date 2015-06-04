// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Storage is responsible for partitioning disks, creating RAID arrays,
// formatting partitions, writing files, writing systemd units, and writing
// network units.

package exec

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"syscall"

	"github.com/coreos/ignition/config"
	"github.com/coreos/ignition/src/exec/util"
	"github.com/coreos/ignition/src/log"
	"github.com/coreos/ignition/src/sgdisk"
	"github.com/coreos/ignition/src/systemd"
)

type storage struct {
	logger *log.Logger
	util.DestDir
}

func (s storage) Run(config config.Config) bool {
	if err := s.createPartitions(config); err != nil {
		s.logger.Crit("create partitions failed: %v", err)
		return false
	}

	if err := s.createRaids(config); err != nil {
		s.logger.Crit("failed to create raids: %v", err)
		return false
	}

	if err := s.createFilesystems(config); err != nil {
		s.logger.Crit("failed to create filesystems: %v", err)
		return false
	}

	if err := s.createUnits(config); err != nil {
		s.logger.Crit("failed to create units: %v", err)
		return false
	}

	return true
}

// waitOnDevices waits for the devices enumerated in devs as a logged operation
// using ctxt for the logging and systemd unit identity.
func (s storage) waitOnDevices(devs []string, ctxt string) error {
	if err := s.logger.LogOp(
		func() error { return systemd.WaitOnDevices(devs, ctxt) },
		"waiting for devices %v", devs,
	); err != nil {
		return fmt.Errorf("failed to wait on %s devs: %v", ctxt, err)
	}
	return nil
}

// createPartitions creates the partitions described in config.Storage.Disks.
func (s storage) createPartitions(config config.Config) error {
	if len(config.Storage.Disks) == 0 {
		return nil
	}
	s.logger.PushPrefix("createPartitions")
	defer s.logger.PopPrefix()

	devs := []string{}
	for _, disk := range config.Storage.Disks {
		devs = append(devs, string(disk.Device))
	}

	if err := s.waitOnDevices(devs, "disks"); err != nil {
		return err
	}

	for _, dev := range config.Storage.Disks {
		err := s.logger.LogOp(func() error {
			op := sgdisk.Begin(s.logger, string(dev.Device))
			if dev.WipeTable {
				s.logger.Info("wiping partition table requested on %q", dev.Device)
				op.WipeTable(true)
			}

			for _, part := range dev.Partitions {
				op.CreatePartition(sgdisk.Partition{
					Number:   part.Number,
					Length:   uint64(part.Size),
					Offset:   uint64(part.Start),
					Label:    string(part.Label),
					TypeGUID: string(part.TypeGUID),
				})
			}

			if err := op.Commit(); err != nil {
				return fmt.Errorf("commit failure: %v", err)
			}
			return nil
		}, "partitioning %q", dev.Device)
		if err != nil {
			return err
		}
	}

	return nil
}

// createRaids creates the raid arrays described in config.Storage.Arrays.
func (s storage) createRaids(config config.Config) error {
	if len(config.Storage.Arrays) == 0 {
		return nil
	}
	s.logger.PushPrefix("createRaids")
	defer s.logger.PopPrefix()

	devs := []string{}
	for _, array := range config.Storage.Arrays {
		for _, dev := range array.Devices {
			devs = append(devs, string(dev))
		}
	}

	if err := s.waitOnDevices(devs, "raids"); err != nil {
		return err
	}

	for _, md := range config.Storage.Arrays {
		// FIXME(vc): this is utterly flummoxed by a preexisting md.Name, the magic of device-resident md metadata really interferes with us.
		// It's as if what ignition really needs is to turn off automagic md probing/running before getting started.
		args := []string{
			"--create", md.Name,
			"--force",
			"--run",
			"--level", md.Level,
			"--raid-devices", fmt.Sprintf("%d", len(md.Devices)-md.Spares),
		}

		if md.Spares > 0 {
			args = append(args, "--spare-devices", fmt.Sprintf("%d", md.Spares))
		}

		for _, dev := range md.Devices {
			args = append(args, string(dev))
		}

		if err := s.logger.LogCmd(
			exec.Command("/sbin/mdadm", args...),
			"creating %q", md.Name,
		); err != nil {
			return fmt.Errorf("mdadm failed: %v", err)
		}
	}

	return nil
}

// createFilesystems creates the filesystems described in config.Storage.Filesystems.
func (s storage) createFilesystems(config config.Config) error {
	if len(config.Storage.Filesystems) == 0 {
		return nil
	}
	s.logger.PushPrefix("createFilesystems")
	defer s.logger.PopPrefix()

	devs := []string{}
	for _, fs := range config.Storage.Filesystems {
		devs = append(devs, string(fs.Device))
	}

	if err := s.waitOnDevices(devs, "filesystems"); err != nil {
		return err
	}

	for _, fs := range config.Storage.Filesystems {
		if fs.Initialize {
			mkfs := ""
			args := []string(fs.Options)
			switch fs.Format {
			case "btrfs":
				mkfs = "/sbin/mkfs.btrfs"
				args = append(args, "--force")
			case "ext4":
				mkfs = "/sbin/mkfs.ext4"
				args = append(args, "-F")
			default:
				return fmt.Errorf("unsupported filesystem format: %q", fs.Format)
			}

			args = append(args, string(fs.Device))
			if err := s.logger.LogCmd(
				exec.Command(mkfs, args...),
				"creating %q filesystem on %q",
				fs.Format, string(fs.Device),
			); err != nil {
				return fmt.Errorf("failed to run %q: %v %v", mkfs, err, args)
			}
		}

		if err := s.createFiles(fs); err != nil {
			return fmt.Errorf("failed to create files %q: %v", fs.Device, err)
		}
	}

	return nil
}

// createFiles creates any files listed for the filesystem in fs.Files.
func (s storage) createFiles(fs config.Filesystem) error {
	if len(fs.Files) == 0 {
		return nil
	}
	s.logger.PushPrefix("createFiles")
	defer s.logger.PopPrefix()

	mnt, err := ioutil.TempDir("", "ignition-files")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %v", err)
	}
	defer os.Remove(mnt)

	dev := string(fs.Device)
	format := string(fs.Format)

	if err := s.logger.LogOp(
		func() error { return syscall.Mount(dev, mnt, format, 0, "") },
		"mounting %q at %q", dev, mnt,
	); err != nil {
		return fmt.Errorf("failed to mount device %q at %q: %v", dev, mnt, err)
	}
	defer s.logger.LogOp(
		func() error { return syscall.Unmount(mnt, 0) },
		"unmounting %q at %q", dev, mnt,
	)

	dest := util.DestDir(mnt)
	for _, f := range fs.Files {
		if err := s.logger.LogOp(
			func() error { return dest.WriteFile(&f) },
			"writing file %q", string(f.Path),
		); err != nil {
			return fmt.Errorf("failed to create file %q: %v", f.Path, err)
		}
	}

	return nil
}

// createUnits creates the units listed under systemd.units and networkd.units.
func (s storage) createUnits(config config.Config) error {
	for _, unit := range config.Systemd.Units {
		if err := s.writeSystemdUnit(unit); err != nil {
			return err
		}
		if unit.Enable {
			if err := s.logger.LogOp(
				func() error { return s.EnableUnit(unit) },
				"enabling unit %q", unit.Name,
			); err != nil {
				return err
			}
		}
		if unit.Mask {
			if err := s.logger.LogOp(
				func() error { return s.MaskUnit(unit) },
				"masking unit %q", unit.Name,
			); err != nil {
				return err
			}
		}
	}
	for _, unit := range config.Networkd.Units {
		if err := s.writeNetworkdUnit(unit); err != nil {
			return err
		}
	}
	return nil
}

// writeSystemdUnit creates the specified unit and any dropins for that unit.
// If the contents of the unit or are empty, the unit is not created. The same
// applies to the unit's dropins.
func (s storage) writeSystemdUnit(unit config.SystemdUnit) error {
	return s.logger.LogOp(func() error {
		for _, dropin := range unit.DropIns {
			if dropin.Contents == "" {
				continue
			}

			f := util.FileFromUnitDropin(unit, dropin)
			if err := s.logger.LogOp(
				func() error { return s.WriteFile(f) },
				"writing dropin %q at %q", dropin.Name, f.Path,
			); err != nil {
				return err
			}
		}

		if unit.Contents == "" {
			return nil
		}

		f := util.FileFromSystemdUnit(unit)
		if err := s.logger.LogOp(
			func() error { return s.WriteFile(f) },
			"writing unit %q at %q", unit.Name, f.Path,
		); err != nil {
			return err
		}

		return nil
	}, "writing unit %q", unit.Name)
}

// writeNetworkdUnit creates the specified unit. If the contents of the unit or
// are empty, the unit is not created.
func (s storage) writeNetworkdUnit(unit config.NetworkdUnit) error {
	return s.logger.LogOp(func() error {
		if unit.Contents == "" {
			return nil
		}

		f := util.FileFromNetworkdUnit(unit)
		if err := s.logger.LogOp(
			func() error { return s.WriteFile(f) },
			"writing unit %q at %q", unit.Name, f.Path,
		); err != nil {
			return err
		}

		return nil
	}, "writing unit %q", unit.Name)
}
