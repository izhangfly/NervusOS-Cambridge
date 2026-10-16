package protocheck

import (
	"errors"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func TestGateSupportFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta *ipcv1.MethodMeta

		operationsWired bool
		want            error
	}{
		{name: "nil metadata", want: ErrNilMethodMeta},
		{

			name: "operation without manager",
			meta: &ipcv1.MethodMeta{ReturnsOperation: true},
			want: ErrOperationUnsupported,
		},
		{
			name:            "operation with manager",
			meta:            &ipcv1.MethodMeta{ReturnsOperation: true},
			operationsWired: true,
		},
		{
			// Caller eligibility belongs to GateUserConfirmation; this function only answers what's wired up.
			// This pins "don't move the eligibility check back here": moving it back would make the Provider-report
			// re-check path (ValidateProviderStatus, which has no caller) refuse everything
			name: "confirmation is not a support question",
			meta: &ipcv1.MethodMeta{NeedsUserConfirmation: true},
		},
		{name: "supported", meta: &ipcv1.MethodMeta{}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := GateSupport(test.meta, test.operationsWired)
			if test.want == nil {
				if err != nil {
					t.Fatalf("GateSupport() error = %v", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("GateSupport() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestGateUserConfirmation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta *ipcv1.MethodMeta

		callerIsConfirmationUI bool
		want                   error
	}{
		{name: "nil metadata", want: ErrNilMethodMeta},
		{
			// Regression pin for deadlock B: an ordinary caller triggering a confirmation-required method must be refused,
			// or any app could bypass the confirmation screen and install packages directly
			name: "confirmation without authority",
			meta: &ipcv1.MethodMeta{NeedsUserConfirmation: true},
			want: ErrConfirmationUnauthorized,
		},
		{
			// Pin for resolving deadlock B: the confirmation UI itself must be let through,
			// or installing over IPC is blocked for everyone (as it was before this change)
			name:                   "confirmation by the confirmation ui",
			meta:                   &ipcv1.MethodMeta{NeedsUserConfirmation: true},
			callerIsConfirmationUI: true,
		},
		{
			// Methods that don't need confirmation aren't affected by the caller lacking that permission
			name: "method needs no confirmation",
			meta: &ipcv1.MethodMeta{},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := GateUserConfirmation(test.meta, test.callerIsConfirmationUI)
			if test.want == nil {
				if err != nil {
					t.Fatalf("GateUserConfirmation() error = %v", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("GateUserConfirmation() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestValidateProviderStatus(t *testing.T) {
	t.Parallel()

	supported := &ipcv1.MethodMeta{}
	tests := []struct {
		name            string
		meta            *ipcv1.MethodMeta
		outcome         ProviderOutcome
		code            ipcv1.StatusCode
		operationsWired bool
		want            error
	}{
		{
			name: "success OK",
			meta: supported, outcome: ProviderOutcomeSuccess,
			code: ipcv1.StatusCode_STATUS_CODE_OK,
		},
		{

			name: "plain method cannot accept",
			meta: supported, outcome: ProviderOutcomeSuccess,
			code: ipcv1.StatusCode_STATUS_CODE_ACCEPTED,
			want: ErrProviderStatus,
		},
		{

			name:            "operation method may accept",
			meta:            &ipcv1.MethodMeta{ReturnsOperation: true},
			outcome:         ProviderOutcomeSuccess,
			code:            ipcv1.StatusCode_STATUS_CODE_ACCEPTED,
			operationsWired: true,
		},
		{
			name: "failure invalid argument",
			meta: supported, outcome: ProviderOutcomeFailure,
			code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
		},
		{
			name: "failure cancelled",
			meta: supported, outcome: ProviderOutcomeFailure,
			code: ipcv1.StatusCode_STATUS_CODE_CANCELLED,
		},
		{
			name: "provider cannot deny permission",
			meta: supported, outcome: ProviderOutcomeFailure,
			code: ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			want: ErrProviderStatus,
		},
		{
			name: "provider cannot unauthenticate",
			meta: supported, outcome: ProviderOutcomeFailure,
			code: ipcv1.StatusCode_STATUS_CODE_UNAUTHENTICATED,
			want: ErrProviderStatus,
		},
		{
			name: "zero status",
			meta: supported, outcome: ProviderOutcomeFailure,
			code: ipcv1.StatusCode_STATUS_CODE_UNSPECIFIED,
			want: ErrProviderStatus,
		},
		{
			name: "unknown outcome",
			meta: supported, outcome: ProviderOutcomeUnspecified,
			code: ipcv1.StatusCode_STATUS_CODE_OK,
			want: ErrProviderStatus,
		},
		{
			name:    "unsupported operation is rejected before status",
			meta:    &ipcv1.MethodMeta{ReturnsOperation: true},
			outcome: ProviderOutcomeSuccess,
			code:    ipcv1.StatusCode_STATUS_CODE_ACCEPTED,
			want:    ErrOperationUnsupported,
		},
		{
			// The Provider report path doesn't re-check caller eligibility: getting here means
			// GateUserConfirmation already let it through at dispatch, and refusing again would only stop a legitimate call
			// started by the confirmation UI from getting its result
			name:    "confirmation result is not re-litigated here",
			meta:    &ipcv1.MethodMeta{NeedsUserConfirmation: true},
			outcome: ProviderOutcomeSuccess,
			code:    ipcv1.StatusCode_STATUS_CODE_OK,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateProviderStatus(test.meta, test.outcome, test.code, test.operationsWired)
			if test.want == nil {
				if err != nil {
					t.Fatalf("ValidateProviderStatus() error = %v", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("ValidateProviderStatus() error = %v, want %v", err, test.want)
			}
		})
	}
}
