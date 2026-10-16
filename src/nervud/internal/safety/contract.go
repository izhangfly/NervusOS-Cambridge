package safety

import (
	"errors"
	"time"
)

// Contract is the Safety Contract of an actuator Resource: the time budgets and escalation windows the OEM declares in
// the Provider manifest's `safety_contract`, settled after nervud Policy checks them independently.
//
// v1 only defines the type and validation; parsing the manifest is left until service/Provider are wired.
// The platform doesn't prescribe a uniform stopping time in milliseconds: real numbers come from the robot's risk analysis, braking capability and
// real hardware tests. nervud may tighten the OEM's declarations by risk, but must not be lowered by the OEM below the system safety floor.
type Contract struct {
	// HaltDispatchBudget is the target budget from the atomic latch to submitting the high-priority stop signal.
	// On timeout only a kernel Safety fault is recorded; the Latch stays.
	HaltDispatchBudget time.Duration

	// ProviderAcceptTimeout is the deadline for the Provider to confirm it accepted the stop request (HaltAccepted).
	// On timeout -> mark the Provider/Resource FAULT, stop ordinary Dispatch, hand over to the independent MCU/watchdog/power cut.
	ProviderAcceptTimeout time.Duration

	// DeviceStopAckTimeout is the deadline for the MCU/controller to confirm it received the stop or that output is off (OUTPUT_DISABLED).
	// On timeout -> stop motion heartbeats and wait for the independent watchdog; with a hard cut-off, escalate by Policy.
	DeviceStopAckTimeout time.Duration

	// StandstillTimeout is the deadline for getting trustworthy physical standstill evidence.
	// On timeout -> the Latch stays, STANDSTILL_TIMEOUT is reported, and power cut / alarm / human handling continues by device policy.
	StandstillTimeout time.Duration

	// MCUWatchdogTimeout is the longest link-loss window before the MCU stops locally after the Host/Provider fails.
	// It must be independent of Linux/the Provider and smaller than the longest loss-of-control window the device's risk analysis allows.
	MCUWatchdogTimeout time.Duration

	// StandstillConfirmationSupported: whether this device can provide trustworthy standstill evidence (encoders / speed estimation).
	// When false, stop progress is capped at OUTPUT_DISABLED.
	StandstillConfirmationSupported bool
}

// DefaultContract returns a conservative set of fail-closed placeholder defaults. Real hardware must override them with measurements.
//
// ProviderAcceptTimeout is 100ms to line up with the example's requested_ack_deadline_ms;
// MCUWatchdogTimeout's 300ms is only an [ADVX-LEGACY] prototype parameter, not a platform default or a safety certification guarantee.
func DefaultContract() Contract {
	return Contract{
		HaltDispatchBudget:              5 * time.Millisecond,
		ProviderAcceptTimeout:           100 * time.Millisecond,
		DeviceStopAckTimeout:            200 * time.Millisecond,
		StandstillTimeout:               1 * time.Second,
		MCUWatchdogTimeout:              300 * time.Millisecond,
		StandstillConfirmationSupported: false,
	}
}

// Validate does structural checks: every budget is positive, and the escalation windows are monotonic
//
//	(accept <= deviceAck <= standstill).
//
// Checking that nothing falls below the Resource's safety floor needs a Resource risk model; that's left until resource lands,
// for Policy to do with something like ValidateAgainstFloor. This round only guarantees the Contract is self-consistent.
func (c Contract) Validate() error {
	if c.HaltDispatchBudget <= 0 || c.ProviderAcceptTimeout <= 0 || c.DeviceStopAckTimeout <= 0 ||
		c.StandstillTimeout <= 0 || c.MCUWatchdogTimeout <= 0 {
		return errors.New("safety: all contract budgets must be positive")
	}
	if c.ProviderAcceptTimeout > c.DeviceStopAckTimeout {
		return errors.New("safety: provider_accept_timeout must not exceed device_stop_ack_timeout")
	}
	if c.DeviceStopAckTimeout > c.StandstillTimeout {
		return errors.New("safety: device_stop_ack_timeout must not exceed standstill_timeout")
	}
	return nil
}
