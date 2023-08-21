//go:build windows
// +build windows

package hyperv

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/containers/libhvee/pkg/hypervctl"
	"github.com/containers/podman/v4/pkg/machine"
	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
)

type HyperVVirtualization struct {
	machine.Virtualization
}

func VirtualizationProvider() machine.VirtProvider {
	return &HyperVVirtualization{
		machine.NewVirtualization(machine.HyperV, machine.Zip, machine.Vhdx),
	}
}

func (v HyperVVirtualization) CheckExclusiveActiveVM() (bool, string, error) {
	vmm := hypervctl.NewVirtualMachineManager()
	// Use of GetAll is OK here because we do not want to use the same name
	// as something already *actually* configured in hyperv
	vms, err := vmm.GetAll()
	if err != nil {
		return false, "", err
	}
	for _, vm := range vms {
		if vm.IsStarting() || vm.State() == hypervctl.Enabled {
			return true, vm.ElementName, nil
		}
	}
	return false, "", nil
}

func (v HyperVVirtualization) IsValidVMName(name string) (bool, error) {
	// We check both the local filesystem and hyperv for the valid name
	mm := HyperVMachine{Name: name}
	configDir, err := machine.GetConfDir(v.VMType())
	if err != nil {
		return false, err
	}
	if err := mm.loadHyperVMachineFromJSON(configDir); err != nil {
		return false, err
	}
	// The name is valid for the local filesystem
	if _, err := hypervctl.NewVirtualMachineManager().GetMachine(name); err != nil {
		return false, err
	}
	// The lookup in hyperv worked, so it is also valid there
	return true, nil
}

func (v HyperVVirtualization) List(opts machine.ListOptions) ([]*machine.ListResponse, error) {
	mms, err := v.loadFromLocalJson()
	if err != nil {
		return nil, err
	}

	var response []*machine.ListResponse
	vmm := hypervctl.NewVirtualMachineManager()

	for _, mm := range mms {
		vm, err := vmm.GetMachine(mm.Name)
		if err != nil {
			return nil, err
		}
		mlr := machine.ListResponse{
			Name:           mm.Name,
			CreatedAt:      mm.Created,
			LastUp:         mm.LastUp,
			Running:        vm.State() == hypervctl.Enabled,
			Starting:       vm.IsStarting(),
			Stream:         mm.ImageStream,
			VMType:         machine.HyperVVirt.String(),
			CPUs:           mm.CPUs,
			Memory:         mm.Memory * units.MiB,
			DiskSize:       mm.DiskSize * units.GiB,
			Port:           mm.Port,
			RemoteUsername: mm.RemoteUsername,
			IdentityPath:   mm.IdentityPath,
		}
		response = append(response, &mlr)
	}
	return response, err
}

func (v HyperVVirtualization) LoadVMByName(name string) (machine.VM, error) {
	m := &HyperVMachine{Name: name}
	return m.loadFromFile()
}

func (v HyperVVirtualization) NewMachine(opts machine.InitOptions) (machine.VM, error) {
	m := HyperVMachine{Name: opts.Name}
	if len(opts.ImagePath) < 1 {
		return nil, errors.New("must define --image-path for hyperv support")
	}

	configDir, err := machine.GetConfDir(machine.HyperVVirt)
	if err != nil {
		return nil, err
	}

	configPath, err := machine.NewMachineFile(getVMConfigPath(configDir, opts.Name), nil)
	if err != nil {
		return nil, err
	}

	m.ConfigPath = *configPath

	ignitionPath, err := machine.NewMachineFile(filepath.Join(configDir, m.Name)+".ign", nil)
	if err != nil {
		return nil, err
	}
	m.IgnitionFile = *ignitionPath

	// Set creation time
	m.Created = time.Now()

	dataDir, err := machine.GetDataDir(machine.HyperVVirt)
	if err != nil {
		return nil, err
	}

	// Set the proxy pid file
	gvProxyPid, err := machine.NewMachineFile(filepath.Join(dataDir, "gvproxy.pid"), nil)
	if err != nil {
		return nil, err
	}
	m.GvProxyPid = *gvProxyPid

	// Acquire the image
	imagePath, imageStream, err := v.acquireVMImage(opts)
	if err != nil {
		return nil, err
	}

	// assign values to machine
	m.ImagePath = *imagePath
	m.ImageStream = imageStream

	config := hypervctl.HardwareConfig{
		CPUs:     uint16(opts.CPUS),
		DiskPath: imagePath.GetPath(),
		DiskSize: opts.DiskSize,
		Memory:   int32(opts.Memory),
	}

	// Write the json configuration file which will be loaded by
	// LoadByName
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(m.ConfigPath.GetPath(), b, 0644); err != nil {
		return nil, err
	}

	vmm := hypervctl.NewVirtualMachineManager()
	if err := vmm.NewVirtualMachine(opts.Name, &config); err != nil {
		return nil, err
	}
	return v.LoadVMByName(opts.Name)
}

// acquireVMImage determines if the image is already in a FCOS stream. If so,
// retrieves the image path of the uncompressed file. Otherwise, the user has
// provided an alternative image, so we set the image path and download the image.
func (v HyperVVirtualization) acquireVMImage(opts machine.InitOptions) (*machine.VMFile, string, error) {
	imageStream := opts.ImagePath
	var imagePath *machine.VMFile
	switch opts.ImagePath {
	// TODO these need to be re-typed as FCOSStreams
	case machine.Testing.String(), machine.Next.String(), machine.Stable.String(), "":
		// Get image as usual
		vp := VirtualizationProvider()
		dd, err := machine.NewFcosDownloader(machine.HyperVVirt, opts.Name, machine.FCOSStreamFromString(imageStream), vp)
		if err != nil {
			return nil, "", err
		}

		uncompressedFile, err := machine.NewMachineFile(dd.Get().LocalUncompressedFile, nil)
		if err != nil {
			return nil, "", err
		}

		imagePath = uncompressedFile
		if err := machine.DownloadImage(dd); err != nil {
			return nil, "", err
		}
	default:
		// The user has provided an alternate image which can be a file path
		// or URL.
		imageStream = "custom"
		altImagePath, err := machine.AcquireAlternateImage(opts.Name, vmtype, opts)
		if err != nil {
			return nil, "", err
		}
		imagePath = altImagePath
	}
	return imagePath, imageStream, nil
}

func (v HyperVVirtualization) RemoveAndCleanMachines() error {
	// Error handling used here is following what qemu did
	var (
		prevErr error
	)

	// The next three info lookups must succeed or we return
	mms, err := v.loadFromLocalJson()
	if err != nil {
		return err
	}

	configDir, err := machine.GetConfDir(vmtype)
	if err != nil {
		return err
	}

	dataDir, err := machine.GetDataDir(vmtype)
	if err != nil {
		return err
	}

	vmm := hypervctl.NewVirtualMachineManager()
	for _, mm := range mms {
		vm, err := vmm.GetMachine(mm.Name)
		if err != nil {
			prevErr = handlePrevError(err, prevErr)
		}

		// If the VM is not stopped, we need to stop it
		// TODO stop might not be enough if the state is dorked. we may need
		// something like forceoff hard switch
		if vm.State() != hypervctl.Disabled {
			if err := vm.Stop(); err != nil {
				prevErr = handlePrevError(err, prevErr)
			}
		}
		if err := vm.Remove(mm.ImagePath.GetPath()); err != nil {
			prevErr = handlePrevError(err, prevErr)
		}
		if err := mm.ReadyHVSock.Remove(); err != nil {
			prevErr = handlePrevError(err, prevErr)
		}
		if err := mm.NetworkHVSock.Remove(); err != nil {
			prevErr = handlePrevError(err, prevErr)
		}
	}

	// Nuke the config and dataDirs
	if err := os.RemoveAll(configDir); err != nil {
		prevErr = handlePrevError(err, prevErr)
	}
	if err := os.RemoveAll(dataDir); err != nil {
		prevErr = handlePrevError(err, prevErr)
	}
	return prevErr
}

func (v HyperVVirtualization) VMType() machine.VMType {
	return vmtype
}

func (v HyperVVirtualization) loadFromLocalJson() ([]*HyperVMachine, error) {
	var (
		jsonFiles []string
		mms       []*HyperVMachine
	)
	configDir, err := machine.GetConfDir(v.VMType())
	if err != nil {
		return nil, err
	}
	if err := filepath.WalkDir(configDir, func(input string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if filepath.Ext(d.Name()) == ".json" {
			jsonFiles = append(jsonFiles, input)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	for _, jsonFile := range jsonFiles {
		mm := HyperVMachine{}
		if err := mm.loadHyperVMachineFromJSON(jsonFile); err != nil {
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		mms = append(mms, &mm)
	}
	return mms, nil
}

func handlePrevError(e, prevErr error) error {
	if prevErr != nil {
		logrus.Error(e)
	}
	return e
}