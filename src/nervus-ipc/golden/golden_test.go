package golden_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervus-ipc/golden"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// -update rewrites the committed .binpb files and vectors.json. Normally (in CI) the flag is absent,
// and only the assertions run; with the flag, the deterministic bytes the Go side builds are written to disk as the cross-language source of truth.
var update = flag.Bool("update", false, "rewrite golden .binpb and vectors.json")

// goldenDir returns testdata/golden under the repo root. The test CWD is this package's directory
// (golden); walk up to the directory containing buf.yaml, i.e. the repo root.
func goldenDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "buf.yaml")); err == nil {
			return filepath.Join(dir, "testdata", "golden")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root (buf.yaml) not found above CWD")
		}
		dir = parent
	}
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// manifestEntry is one entry of vectors.json: a language-neutral list of vectors for the JVM side to load by name.
type manifestEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Doc  string `json:"doc"`
}

// TestGoldenUpdate writes to disk under -update; otherwise it skips.
func TestGoldenUpdate(t *testing.T) {
	if !*update {
		t.Skip("run with -update to regenerate golden files")
	}
	dir := goldenDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var manifest []manifestEntry
	for _, v := range golden.Vectors() {
		file := v.Name + ".binpb"
		b := marshal(t, v.Build())
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
			t.Fatal(err)
		}
		manifest = append(manifest, manifestEntry{
			Name: v.Name, Kind: string(v.Kind), File: file, Doc: v.Doc,
		})
	}
	mj, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mj = append(mj, '\n')
	if err := os.WriteFile(filepath.Join(dir, "vectors.json"), mj, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d golden vectors + manifest to %s", len(manifest), dir)
}

// TestGoldenBytesMatch is the core gate: each vector built in Go → deterministic serialisation
// must equal the committed .binpb byte for byte. The JVM side asserts the same against the same files,
// so Go and JVM agree byte for byte transitively.
func TestGoldenBytesMatch(t *testing.T) {
	if *update {
		t.Skip("skip assertions during -update")
	}
	dir := goldenDir(t)
	for _, v := range golden.Vectors() {
		t.Run(v.Name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(dir, v.Name+".binpb"))
			if err != nil {
				t.Fatalf("read golden (run `go test -run TestGoldenUpdate -update`): %v", err)
			}
			got := marshal(t, v.Build())
			if string(got) != string(want) {
				t.Errorf("bytes mismatch\n got=%x\nwant=%x", got, want)
			}
		})
	}
}

// TestGoldenDecode asserts the committed bytes can be decoded back with the key fields as expected,
// covering the decoding direction of A5 §3 (an unknown reason keeps the generic code, a malformed detail is normalised, and so on).
func TestGoldenDecode(t *testing.T) {
	dir := goldenDir(t)
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name+".binpb"))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return b
	}

	// response_success_ok
	{
		var env ipcv1.Envelope
		if err := proto.Unmarshal(read("response_success_ok"), &env); err != nil {
			t.Fatal(err)
		}
		s := env.GetResponse().GetSuccess()
		if s.GetCode() != ipcv1.StatusCode_STATUS_CODE_OK || string(s.GetPayload()) != "pong" {
			t.Errorf("response_success_ok decoded wrong: %+v", s)
		}
	}

	// unknown reason: the outer code stays the generic NOT_FOUND, the inner reason=99 is kept as is
	{
		var env ipcv1.Envelope
		if err := proto.Unmarshal(read("resolve_failure_unknown_reason"), &env); err != nil {
			t.Fatal(err)
		}
		f := env.GetResolveEndpointResult().GetFailure()
		if f.GetCode() != ipcv1.StatusCode_STATUS_CODE_NOT_FOUND {
			t.Errorf("unknown reason must keep generic code NOT_FOUND, got %v", f.GetCode())
		}
		var d ipcv1.ResolveEndpointErrorDetail
		if err := proto.Unmarshal(f.GetErrorDetail(), &d); err != nil {
			t.Fatalf("detail must still decode: %v", err)
		}
		if int32(d.GetReason()) != 99 {
			t.Errorf("unknown reason must round-trip as 99, got %d", d.GetReason())
		}
	}

	// malformed detail: outer INTERNAL, detail can't be parsed as ResolveEndpointErrorDetail
	{
		var env ipcv1.Envelope
		if err := proto.Unmarshal(read("failure_malformed_detail_internal"), &env); err != nil {
			t.Fatal(err)
		}
		f := env.GetResponse().GetFailure()
		if f.GetCode() != ipcv1.StatusCode_STATUS_CODE_INTERNAL {
			t.Errorf("malformed detail must be normalized to INTERNAL, got %v", f.GetCode())
		}
	}

	// AcquireControl HUMAN base
	{
		var env ipcv1.Envelope
		if err := proto.Unmarshal(read("acquire_control_human_base"), &env); err != nil {
			t.Fatal(err)
		}
		a := env.GetAcquireControl()
		if a.GetControllerClass() != ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN {
			t.Errorf("controller_class wrong: %v", a.GetControllerClass())
		}
		if a.GetResource().GetType() != "nervus.resource.motion.base" || a.GetResource().GetRole() != "main" {
			t.Errorf("resource wrong: %+v", a.GetResource())
		}
		if a.GetRequestedDeadlineNanos() != 500_000_000 {
			t.Errorf("deadline wrong: %d", a.GetRequestedDeadlineNanos())
		}
	}

	// AcquireControlResult held_by_human: a distinguishable reason
	{
		var env ipcv1.Envelope
		if err := proto.Unmarshal(read("acquire_control_result_held_by_human"), &env); err != nil {
			t.Fatal(err)
		}
		f := env.GetAcquireControlResult().GetFailure()
		if f.GetCode() != ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION {
			t.Errorf("code wrong: %v", f.GetCode())
		}
		var d ipcv1.ControlLeaseErrorDetail
		if err := proto.Unmarshal(f.GetErrorDetail(), &d); err != nil {
			t.Fatal(err)
		}
		if d.GetReason() != ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_HELD_BY_HUMAN {
			t.Errorf("reason must be HELD_BY_HUMAN, got %v", d.GetReason())
		}
	}

	// Dispatch ExecutionContext: the trusted lease proof fields must be kept intact across languages
	{
		var env ipcv1.Envelope
		if err := proto.Unmarshal(read("dispatch_control_execution_context"), &env); err != nil {
			t.Fatal(err)
		}
		d := env.GetDispatch()
		ec := d.GetExecutionContext()
		if env.GetProtocolMinor() != 1 || d.GetRouteId() != 15 ||
			ec.GetLeaseId() != 0xABCD ||
			ec.GetControllerClass() != ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN ||
			ec.GetMotionEpoch() != 3 || ec.GetDeadlineNanos() != 1_234_567_890 ||
			ec.GetCommandSequence() != 17 || ec.GetResourceHandle() != "base.main" ||
			ec.GetResourceGeneration() != 4 {
			t.Errorf("dispatch execution context decoded wrong: envelope=%+v context=%+v", &env, ec)
		}
	}

	// InterfaceSchemaBundle
	{
		var b ipcv1.InterfaceSchemaBundle
		if err := proto.Unmarshal(read("interface_schema_bundle"), &b); err != nil {
			t.Fatal(err)
		}
		if b.GetInterfaceId() != "com.acme.interface.gripper" || b.GetVersion() != 1 {
			t.Errorf("bundle id/version wrong: %+v", &b)
		}
		if b.GetMethodEnumType() != "com.acme.gripper.v1.GripperMethod" {
			t.Errorf("bundle method enum wrong: %q", b.GetMethodEnumType())
		}
	}

	// TransferHandle
	{
		var handle ipcv1.TransferHandle
		if err := proto.Unmarshal(read("transfer_handle_peer"), &handle); err != nil {
			t.Fatal(err)
		}
		if handle.GetRole() != ipcv1.TransferRole_TRANSFER_ROLE_PEER ||
			handle.GetMode() != ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY ||
			len(handle.GetTransferId()) != 16 || len(handle.GetAttachTicket()) != 32 {
			t.Errorf("transfer handle wrong: %+v", &handle)
		}
	}
}
