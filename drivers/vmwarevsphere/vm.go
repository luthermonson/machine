package vmwarevsphere

import (
	"fmt"
	"os"
	"strings"

	"github.com/docker/machine/libmachine/log"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/guest"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func (d *Driver) getFolder(vm *object.VirtualMachine) (string, error) {
	var mvm mo.VirtualMachine
	c, err := d.getSoapClient()
	if err != nil {
		return "", err
	}

	err = c.RetrieveOne(d.getCtx(), vm.Reference(), nil, &mvm)
	if err != nil {
		return "", err
	}

	p := mvm.Summary.Config.VmPathName
	sp := strings.Split(p, " ")
	path := strings.Replace(sp[1], fmt.Sprintf("/%s.vmx", d.MachineName), "", 1)

	return path, nil
}

func (d Driver) fetchVM(vmname string) (*object.VirtualMachine, error) {
	if d.vms[vmname] != nil {
		return d.vms[vmname], nil
	}

	c, err := d.getSoapClient()
	if err != nil {
		return nil, err
	}

	// Create a new finder
	f := find.NewFinder(c.Client, true)
	var vm *object.VirtualMachine

	dc, err := f.DatacenterOrDefault(d.getCtx(), d.Datacenter)
	if err != nil {
		return nil, err
	}

	f.SetDatacenter(dc)

	vmPath := vmname
	if d.Folder != "" {
		vmPath = fmt.Sprintf("%s/%s", d.Folder, vmname)
	}

	vm, err = f.VirtualMachine(d.getCtx(), vmPath)
	if err != nil {
		return nil, err
	}

	d.vms[vmname] = vm
	return vm, nil
}

func (d Driver) addNetworks(vm *object.VirtualMachine, networks map[string]object.NetworkReference) error {
	if len(networks) <= 0 {
		return nil
	}

	devices, _ := vm.Device(d.getCtx())
	for _, v := range devices {
		dev := v.GetVirtualDevice()
		if strings.Contains(dev.DeviceInfo.GetDescription().Label, "Network adapter") {
			//remove old networks
			vm.RemoveDevice(d.getCtx(), false, dev)
		}
	}

	var add []types.BaseVirtualDevice
	for _, netName := range d.Networks {
		backing, err := networks[netName].EthernetCardBackingInfo(d.getCtx())
		if err != nil {
			return err
		}

		netdev, err := object.EthernetCardTypes().CreateEthernetCard("vmxnet3", backing)
		if err != nil {
			return err
		}

		log.Infof("adding network: %s", netName)
		add = append(add, netdev)
	}

	if err := vm.AddDevice(d.getCtx(), add...); err != nil {
		return err
	}

	return nil
}

func (d *Driver) provisionVm(vm *object.VirtualMachine) error {
	log.Infof("Provisioning certs and ssh keys...")

	c, err := d.getSoapClient()
	if err != nil {
		return err
	}

	// Generate a tar keys bundle
	if err := d.generateKeyBundle(); err != nil {
		return err
	}

	opman := guest.NewOperationsManager(c.Client, vm.Reference())

	fileman, err := opman.FileManager(d.getCtx())
	if err != nil {
		return err
	}

	src := d.ResolveStorePath("userdata.tar")
	s, err := os.Stat(src)
	if err != nil {
		return err
	}

	auth := NewAuthFlag(d.SSHUser, d.SSHPassword)
	flag := FileAttrFlag{}
	flag.SetPerms(0, 0, 660)

	tmpDir, err := fileman.CreateTemporaryDirectory(d.getCtx(), auth.Auth(), "docker_", "", "/tmp")
	if err != nil {
		return err
	}

	url, err := fileman.InitiateFileTransferToGuest(d.getCtx(), auth.Auth(), tmpDir+"/userdata.tar", flag.Attr(), s.Size(), true)
	if err != nil {
		return err
	}

	u, err := c.Client.ParseURL(url)
	if err != nil {
		return err
	}
	if err = c.Client.UploadFile(d.getCtx(), src, u, nil); err != nil {
		return err
	}

	procman, err := opman.ProcessManager(d.getCtx())
	if err != nil {
		return err
	}

	cmds := []string{
		fmt.Sprintf("/bin/tar xvf %s/userdata.tar -C %s", tmpDir, tmpDir),
		fmt.Sprintf("/bin/chown -R %s:%s %s", d.SSHUser, d.SSHUserGroup, tmpDir),
		"/bin/mkdir -p /var/lib/boot2docker",
		fmt.Sprintf("/bin/cp %s/userdata.tar /var/lib/boot2docker/userdata.tar", tmpDir),
		fmt.Sprintf("/bin/mkdir -p /home/%s/.ssh", d.SSHUser),
		fmt.Sprintf("/bin/cp %s/.ssh/* /home/%s/.ssh", tmpDir, d.SSHUser), //copy keys to user homedir
	}

	for _, cmd := range cmds {
		if _, err := d.exec(procman, cmd); err != nil {
			return err
		}
	}

	return nil
}

func (d Driver) addConfigParams(vm *object.VirtualMachine) error {
	var opts []types.BaseOptionValue
	if len(d.CfgParams) > 0 {
		for _, param := range d.CfgParams {
			v := strings.SplitN(param, "=", 2)
			key := v[0]
			value := ""
			if len(v) > 1 {
				value = v[1]
			}
			fmt.Printf("Setting %s to %s\n", key, value)
			opts = append(opts, &types.OptionValue{
				Key:   key,
				Value: value,
			})
		}
	}

	return d.applyOpts(vm, opts)
}

func (d Driver) applyOpts(vm *object.VirtualMachine, opts []types.BaseOptionValue) error {
	if len(opts) == 0 {
		return nil
	}

	task, err := vm.Reconfigure(d.getCtx(), types.VirtualMachineConfigSpec{
		ExtraConfig: opts,
	})

	if err != nil {
		return err
	}

	return task.Wait(d.getCtx())
}
