// Copyright (c) Red Hat, Inc.
// SPDX-License-Identifier: MPL-2.0

package common

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"
)

const (
	testNamespace = "test-ns"
	testVMName    = "test-vm"
)

// fakeCommunicator is a minimal packer.Communicator whose remote command exits
// with a configurable status (and optional transport error), so shutdown
// behavior can be exercised without a real guest.
type fakeCommunicator struct {
	exitStatus int
	startErr   error
	commands   []string
	onStart    func()
}

func (c *fakeCommunicator) Start(_ context.Context, rc *packer.RemoteCmd) error {
	c.commands = append(c.commands, rc.Command)
	if c.onStart != nil {
		c.onStart()
	}
	rc.SetExited(c.exitStatus)
	return c.startErr
}

func (c *fakeCommunicator) Upload(string, io.Reader, *os.FileInfo) error { return nil }
func (c *fakeCommunicator) UploadDir(string, string, []string) error     { return nil }
func (c *fakeCommunicator) Download(string, io.Writer) error             { return nil }
func (c *fakeCommunicator) DownloadDir(string, string, []string) error   { return nil }

// shutdownClient adapts the KubeVirt fake clientset to the ShutdownClient
// interface StepShutdown consumes.
type shutdownClient struct{ cs *kubevirtfake.Clientset }

func (s shutdownClient) VirtualMachine(ns string) kubecli.VirtualMachineInterface {
	return s.cs.KubevirtV1().VirtualMachines(ns)
}

func (s shutdownClient) VirtualMachineInstance(ns string) kubecli.VirtualMachineInstanceInterface {
	return s.cs.KubevirtV1().VirtualMachineInstances(ns)
}

func newState(comm packer.Communicator) *multistep.BasicStateBag {
	state := new(multistep.BasicStateBag)
	state.Put("ui", &packer.BasicUi{Reader: strings.NewReader(""), Writer: io.Discard, ErrorWriter: io.Discard})
	if comm != nil {
		state.Put("communicator", comm)
	}
	return state
}

func createVM(t *testing.T, cs *kubevirtfake.Clientset, status v1.VirtualMachinePrintableStatus) {
	t.Helper()
	_, err := cs.KubevirtV1().VirtualMachines(testNamespace).Create(context.Background(), &v1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace},
		Status:     v1.VirtualMachineStatus{PrintableStatus: status},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create VM: %v", err)
	}
}

func createVMI(t *testing.T, cs *kubevirtfake.Clientset, phase v1.VirtualMachineInstancePhase) {
	t.Helper()
	_, err := cs.KubevirtV1().VirtualMachineInstances(testNamespace).Create(context.Background(), &v1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: testVMName, Namespace: testNamespace},
		Status:     v1.VirtualMachineInstanceStatus{Phase: phase},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create VMI: %v", err)
	}
}

func newStep(cs *kubevirtfake.Clientset, command string) *StepShutdown {
	return &StepShutdown{
		Client:          shutdownClient{cs: cs},
		Name:            testVMName,
		Namespace:       testNamespace,
		ShutdownCommand: command,
		ShutdownTimeout: 2 * time.Second,
		PollInterval:    5 * time.Millisecond,
	}
}

func TestShutdownNoCommandIsNoOp(t *testing.T) {
	comm := &fakeCommunicator{}
	step := newStep(kubevirtfake.NewSimpleClientset(), "")

	if action := step.Run(context.Background(), newState(comm)); action != multistep.ActionContinue {
		t.Fatalf("action = %v, want Continue", action)
	}
	if len(comm.commands) != 0 {
		t.Fatalf("expected no command execution, got %v", comm.commands)
	}
}

func TestShutdownContinuesOnVMISucceeded(t *testing.T) {
	cs := kubevirtfake.NewSimpleClientset()
	createVM(t, cs, v1.VirtualMachineStatusRunning)
	createVMI(t, cs, v1.Succeeded)

	comm := &fakeCommunicator{}
	step := newStep(cs, "shutdown")
	if action := step.Run(context.Background(), newState(comm)); action != multistep.ActionContinue {
		t.Fatalf("action = %v, want Continue", action)
	}
	if len(comm.commands) != 1 || comm.commands[0] != "shutdown" {
		t.Fatalf("expected shutdown command to run once, got %v", comm.commands)
	}
}

func TestShutdownContinuesWhenVMIGoneAndVMStopped(t *testing.T) {
	cs := kubevirtfake.NewSimpleClientset()
	createVM(t, cs, v1.VirtualMachineStatusStopped) // no VMI created

	step := newStep(cs, "shutdown")
	if action := step.Run(context.Background(), newState(&fakeCommunicator{})); action != multistep.ActionContinue {
		t.Fatalf("action = %v, want Continue", action)
	}
}

func TestShutdownHaltsOnVMIFailed(t *testing.T) {
	cs := kubevirtfake.NewSimpleClientset()
	createVM(t, cs, v1.VirtualMachineStatusRunning)
	createVMI(t, cs, v1.Failed)

	state := newState(&fakeCommunicator{})
	if action := newStep(cs, "shutdown").Run(context.Background(), state); action != multistep.ActionHalt {
		t.Fatalf("action = %v, want Halt", action)
	}
	if err, ok := state.GetOk("error"); !ok || !strings.Contains(err.(error).Error(), "Failed phase") {
		t.Fatalf("expected Failed-phase error, got %v", state.Get("error"))
	}
}

func TestShutdownHaltsWhenGuestNeverStops(t *testing.T) {
	cs := kubevirtfake.NewSimpleClientset()
	createVM(t, cs, v1.VirtualMachineStatusRunning)
	createVMI(t, cs, v1.Running)

	state := newState(&fakeCommunicator{})
	if action := newStep(cs, "shutdown").Run(context.Background(), state); action != multistep.ActionHalt {
		t.Fatalf("action = %v, want Halt on timeout", action)
	}
	if err, ok := state.GetOk("error"); !ok || !strings.Contains(err.(error).Error(), "did not shut down") {
		t.Fatalf("expected a shutdown timeout error, got %v", state.Get("error"))
	}
}

func TestShutdownHaltsWithoutErrorWhenCancelled(t *testing.T) {
	cs := kubevirtfake.NewSimpleClientset()
	createVM(t, cs, v1.VirtualMachineStatusRunning)
	createVMI(t, cs, v1.Running)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	state := newState(&fakeCommunicator{})
	if action := newStep(cs, "shutdown").Run(ctx, state); action != multistep.ActionHalt {
		t.Fatalf("action = %v, want Halt", action)
	}
	if _, ok := state.GetOk("error"); ok {
		t.Fatalf("a cancelled build must not record an error, got %v", state.Get("error"))
	}
}

func TestShutdownHaltsWithoutCommunicator(t *testing.T) {
	step := newStep(kubevirtfake.NewSimpleClientset(), "shutdown")
	state := newState(nil) // no communicator in state
	if action := step.Run(context.Background(), state); action != multistep.ActionHalt {
		t.Fatalf("action = %v, want Halt", action)
	}
}

func TestShutdownToleratesDisconnectExitStatus(t *testing.T) {
	cs := kubevirtfake.NewSimpleClientset()
	createVM(t, cs, v1.VirtualMachineStatusRunning)
	createVMI(t, cs, v1.Succeeded)

	comm := &fakeCommunicator{exitStatus: packer.CmdDisconnect}
	if action := newStep(cs, "shutdown").Run(context.Background(), newState(comm)); action != multistep.ActionContinue {
		t.Fatalf("action = %v, want Continue on disconnect exit", action)
	}
}

func TestShutdownSwitchesToRerunOnFailureBeforeTheCommand(t *testing.T) {
	cs := kubevirtfake.NewSimpleClientset()
	createVM(t, cs, v1.VirtualMachineStatusRunning)
	createVMI(t, cs, v1.Succeeded)

	var strategyAtCommand v1.VirtualMachineRunStrategy
	comm := &fakeCommunicator{onStart: func() {
		vm, err := cs.KubevirtV1().VirtualMachines(testNamespace).Get(context.Background(), testVMName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("get VM: %v", err)
			return
		}
		if vm.Spec.RunStrategy != nil {
			strategyAtCommand = *vm.Spec.RunStrategy
		}
	}}

	if action := newStep(cs, "shutdown").Run(context.Background(), newState(comm)); action != multistep.ActionContinue {
		t.Fatalf("action = %v, want Continue", action)
	}
	if strategyAtCommand != v1.RunStrategyRerunOnFailure {
		t.Fatalf("run strategy when the command ran = %q, want %q", strategyAtCommand, v1.RunStrategyRerunOnFailure)
	}
}
