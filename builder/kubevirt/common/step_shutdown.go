// Copyright (c) Red Hat, Inc.
// SPDX-License-Identifier: MPL-2.0

package common

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
)

const defaultShutdownPollInterval = 5 * time.Second

// ShutdownClient is the subset of the KubeVirt client StepShutdown uses.
type ShutdownClient interface {
	VirtualMachine(namespace string) kubecli.VirtualMachineInterface
	VirtualMachineInstance(namespace string) kubecli.VirtualMachineInstanceInterface
}

// StepShutdown runs the shutdown command through the communicator, then waits
// for the guest to power itself off. It does nothing without a shutdown command.
type StepShutdown struct {
	Client          ShutdownClient
	Name            string
	Namespace       string
	ShutdownCommand string
	ShutdownTimeout time.Duration

	// PollInterval is overridden in tests.
	PollInterval time.Duration
}

func (s *StepShutdown) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	if strings.TrimSpace(s.ShutdownCommand) == "" {
		return multistep.ActionContinue
	}

	ui := state.Get("ui").(packer.Ui)
	comm, ok := state.Get("communicator").(packer.Communicator)
	if !ok {
		return s.halt(ctx, state, ui, errors.New("shutdown_command requires an active communicator"))
	}

	// Under the Always run strategy KubeVirt would restart the VM once the guest
	// powers off. RerunOnFailure keeps it stopped, and still restarts a crash.
	if err := s.setRunStrategy(ctx, v1.RunStrategyRerunOnFailure); err != nil {
		return s.halt(ctx, state, ui, err)
	}

	ui.Say("Executing the guest shutdown command...")
	cmd := &packer.RemoteCmd{Command: s.ShutdownCommand}
	commandErr := cmd.RunWithUi(ctx, comm, ui)
	if commandErr == nil {
		switch exitStatus := cmd.ExitStatus(); exitStatus {
		case 0, packer.CmdDisconnect:
			// The guest may power off before the command reports its status.
		default:
			return s.halt(ctx, state, ui, fmt.Errorf("the shutdown command exited with status %d", exitStatus))
		}
	}

	pollInterval := s.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultShutdownPollInterval
	}

	ui.Sayf("Waiting up to %s for the VirtualMachine (%s/%s) to shut down...", s.ShutdownTimeout, s.Namespace, s.Name)
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, s.ShutdownTimeout, true, s.guestPoweredOff); err != nil {
		err = fmt.Errorf("the VirtualMachine (%s/%s) did not shut down: %w", s.Namespace, s.Name, err)
		if commandErr != nil {
			err = fmt.Errorf("%w (the shutdown command returned: %v)", err, commandErr)
		}
		return s.halt(ctx, state, ui, err)
	}

	if commandErr != nil {
		// The connection drops as the guest powers off; the power-off is what counts.
		log.Printf("shutdown command disconnected after guest power-off: %v", commandErr)
	}
	ui.Say("Guest shut down.")
	return multistep.ActionContinue
}

func (s *StepShutdown) setRunStrategy(ctx context.Context, strategy v1.VirtualMachineRunStrategy) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm, err := s.Client.VirtualMachine(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get the VirtualMachine (%s/%s): %w", s.Namespace, s.Name, err)
		}
		vm.Spec.RunStrategy = ptr.To(strategy)
		if _, err := s.Client.VirtualMachine(s.Namespace).Update(ctx, vm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to set the run strategy of the VirtualMachine (%s/%s): %w", s.Namespace, s.Name, err)
		}
		return nil
	})
}

func (s *StepShutdown) guestPoweredOff(ctx context.Context) (bool, error) {
	vm, err := s.Client.VirtualMachine(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get the VirtualMachine (%s/%s) while waiting for it to shut down: %w", s.Namespace, s.Name, err)
	}

	vmi, err := s.Client.VirtualMachineInstance(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// RerunOnFailure briefly has no VMI while it replaces a failed one, so
		// only a stopped VM confirms the shutdown.
		return vm.Status.PrintableStatus == v1.VirtualMachineStatusStopped, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get the VirtualMachineInstance (%s/%s) while waiting for it to shut down: %w", s.Namespace, s.Name, err)
	}

	switch vmi.Status.Phase {
	case v1.Succeeded:
		return true, nil
	case v1.Failed:
		// RerunOnFailure restarts a failed VMI, so capturing now could copy a live disk.
		return false, fmt.Errorf("the VirtualMachineInstance (%s/%s) entered the Failed phase instead of shutting down cleanly", s.Namespace, s.Name)
	default:
		return false, nil
	}
}

// halt records err, unless the build was cancelled: Builder.Run then reports
// the cancellation instead.
func (s *StepShutdown) halt(ctx context.Context, state multistep.StateBag, ui packer.Ui, err error) multistep.StepAction {
	if ctx.Err() == nil {
		state.Put("error", err)
		ui.Error(err.Error())
	}
	return multistep.ActionHalt
}

func (s *StepShutdown) Cleanup(multistep.StateBag) {}
