// Package stdinterface is the central list of every platform standard interface (nervus.interface.*),
// and the single source of truth for building their schema hashes.
//
// # What gap it fills
//
// RegisterEndpoint must carry interface_schema_hash, which the kernel compares byte for byte with the Catalog's copy,
// refusing on a mismatch (nervud internal/endpoint/register.go). Empty values are always refused too; there used to be
// a compatibility bridge letting only pkgmanagerd through with an empty schema, removed once the packaging chain could produce ProviderArtifacts.
//
// A Provider written in Go can compute this value by calling BuildSchemaBundle directly (pkgmanagerd's
// contract.go does so). But the hash is sha256(the deterministically encoded FileDescriptorSet), and
// the JVM side has **no** equivalent of the registry package, while protobuf-java doesn't guarantee byte-for-byte equality with Go's
// Deterministic encoding. So no Provider written in Kotlin/Java could compute this
// value, and so couldn't register any interface; nervus-app-example's HelloService leaving schemaHash empty when exporting an interface,
// and so being refused at report-in under the current rules, was exactly that gap in action.
//
// This package has the Go side compute it once, written as Kotlin constants committed to the repository, and the JVM side only reads them, never computes. Cross-language
// consistency is guaranteed by "reading the same generated output", not by "both sides implementing the algorithm"; the same shape as the golden
// vectors (Go builds -> committed artefacts -> both sides assert against the same bytes).
//
// # Why generate Kotlin source rather than write a binary bundle set
//
// Intuitively you'd reuse the protocol's existing InterfaceSchemaBundleSet, which the JVM can unpack in one line with the generated protobuf
// classes. That was tried and isn't worth it:
//
//	each bundle embeds its own complete FileDescriptorSet (with every transitive dependency), 11 interfaces
//	come to 360 KB in total, while what's really needed is only 11 x 32 bytes. And most of the content repeats across bundles
//	(every interface imports the same ipc/v1 files).
//
// Stripping the descriptor set to keep only the hash is worse: ParseSchemaBundle explicitly requires the descriptor
// set to be non-empty with a sha256 equal to schema_hash. A stripped bundle set would be something that "looks like a
// bundle set but can't be decoded by the protocol", worse than not using the type at all.
//
// Kotlin constants happen to use a pipeline that's already wired but still empty: nervus-app-sdk's
// cloneProtocol copies everything under jvm/protocol/src/main and adds kotlin/ to the source set,
// while buf generate's clean only deletes java/ entirely, so hand-written files under kotlin/ both reach the SDK
// and survive regeneration.
//
// # Defence against drift
//
// TestCommittedKotlinMatches asserts the committed .kt equals byte for byte what this package computes now.
// Changing any standard interface's .proto without regenerating turns CI red. The regeneration command is written in the generated file's header.
package stdinterface

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sort"
	"text/template"

	basemotionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/basemotionv1"
	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	operationv1 "github.com/nervus-os/nervus-ipc/protocol/interface/operationv1"
	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	pkgmanagerv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	resourcedirv1 "github.com/nervus-os/nervus-ipc/protocol/interface/resourcedirv1"
	safetyv1 "github.com/nervus-os/nervus-ipc/protocol/interface/safetyv1"
	transferv1 "github.com/nervus-os/nervus-ipc/protocol/interface/transferv1"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/nervus-os/nervus-ipc/registry"
)

// KotlinTableFile is the relative path of the committed generated file in the repository.
//
// It's under jvm/protocol/src/main/kotlin rather than java: buf generate's clean deletes
// java/ entirely (it's protoc's output directory), and hand-written files there would vanish at the next regeneration.
const KotlinTableFile = "jvm/protocol/src/main/kotlin/io/github/nervusos/ipc/v1/StdInterfaceSchema.kt"

// Interface is one entry in the platform standard interface list.
type Interface struct {
	// ID is the interface_id, which must match the one declared in the .proto's header comment.
	ID string

	// Major is the interface major version. 0 is reserved.
	Major uint32

	// MethodEnum is the enum descriptor carrying method_meta.
	//
	// It takes the generated code's enum rather than writing literals: if the enum is renamed or moved to another file, this won't compile;
	// literals would only show up at run time as a vague hash mismatch.
	MethodEnum protoreflect.EnumDescriptor

	// EventEnum is the enum descriptor carrying event_meta; nil for interfaces that push no events.
	//
	// It **doesn't affect** the schema hash: the event enum is in the same file as the method enum, so it's in the descriptor
	// set anyway (see the notes on BuildSchemaBundleWithEvents). It's filled in faithfully so this list
	// can also serve as an index of "which standard interfaces have events".
	EventEnum protoreflect.EnumDescriptor
}

// Key returns a stable key of the form "<id>@<major>".
func (i Interface) Key() string { return fmt.Sprintf("%s@%d", i.ID, i.Major) }

// All returns every platform standard interface, in a stable order.
//
// New interfaces always **go at the end**. Entries in the generated file are sorted by key (independent of this order), but keeping this
// list append-only, never reordered, lets diffs show only the new line.
//
// This list must stay consistent with nervud internal/catalog's bootstrap. Both take descriptors from the same
// generated code's enums, so drift can only be "one side added a new interface and the other forgot",
// never the same interface producing two hashes.
func All() []Interface {
	return []Interface{
		{
			ID:         "nervus.interface.motion.base",
			Major:      1,
			MethodEnum: basemotionv1.BaseMotionMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.manipulator.arm",
			Major:      1,
			MethodEnum: manipulatorv1.ManipulatorMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.pkg.manager",
			Major:      1,
			MethodEnum: pkgmanagerv1.PackageManagerMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.safety.control",
			Major:      1,
			MethodEnum: safetyv1.SafetyControlMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.transfer.control",
			Major:      1,
			MethodEnum: transferv1.TransferControlMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.resource.directory",
			Major:      1,
			MethodEnum: resourcedirv1.ResourceDirectoryMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.operation.control",
			Major:      1,
			MethodEnum: operationv1.OperationControlMethod(0).Descriptor(),
			EventEnum:  operationv1.OperationControlEvent(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.permission.admin",
			Major:      1,
			MethodEnum: permissionv1.PermissionAdminMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.permission.ui",
			Major:      1,
			MethodEnum: permissionv1.PermissionUiMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.permission.self",
			Major:      1,
			MethodEnum: permissionv1.PermissionSelfMethod(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.camera",
			Major:      1,
			MethodEnum: camerav1.CameraMethod(0).Descriptor(),
			EventEnum:  camerav1.CameraEvent(0).Descriptor(),
		},
		{
			ID:         "nervus.interface.camera.config",
			Major:      1,
			MethodEnum: camerav1.CameraConfigMethod(0).Descriptor(),
			EventEnum:  camerav1.CameraConfigEvent(0).Descriptor(),
		},
	}
}

// Entry is the schema hash computed for one interface.
type Entry struct {
	ID         string
	Major      uint32
	SchemaHash []byte
}

// Key returns a stable key of the form "<id>@<major>".
func (e Entry) Key() string { return fmt.Sprintf("%s@%d", e.ID, e.Major) }

// Entries computes the schema hash of every interface in All(), in **the same order as All()**.
//
// It keeps the input order rather than sorting: callers (including the gate test) can match Entries to Interfaces
// by index. Sorting inside the generated file happens in RenderKotlin, which is a separate thing: the written order
// shouldn't depend on how the list is written, otherwise reordering the list would produce a file with equivalent content but different bytes,
// turning the gate red with no information.
//
// It goes through registry.BuildSchemaBundleWithEvents, **the same constructor** as nervud's bootstrap and every
// Provider's providergen. That's the only guarantee the three parties don't drift:
// any side switching to another algorithm shows up immediately as a hash mismatch.
func Entries() ([]Entry, error) {
	ifaces := All()
	out := make([]Entry, 0, len(ifaces))
	seen := make(map[string]struct{}, len(ifaces))
	for _, iface := range ifaces {
		key := iface.Key()
		if _, dup := seen[key]; dup {
			// The same (id, major) twice would put two constants with the same name in the generated file, and the Kotlin side
			// wouldn't compile, but the error would point at the generated file rather than the list; catching it here is easier
			return nil, fmt.Errorf("stdinterface: duplicate interface %s", key)
		}
		seen[key] = struct{}{}

		bundle, err := registry.BuildSchemaBundleWithEvents(
			iface.ID, iface.Major, iface.MethodEnum, iface.EventEnum)
		if err != nil {
			return nil, fmt.Errorf("stdinterface: build %s: %w", key, err)
		}
		out = append(out, Entry{
			ID:         iface.ID,
			Major:      iface.Major,
			SchemaHash: bundle.GetSchemaHash(),
		})
	}
	return out, nil
}

// kotlinTemplate is the generated file's Kotlin template.
//
// **A separate .kt.tmpl file rather than an inline string literal.** The inline version used to be a whole
// Kotlin source stuffed into a Go backtick string, with an Fprintf loop in the middle assembling map entries;
// two languages' syntax mixed in one literal, which readers had to untangle in their heads first. After splitting it into a template file,
// this .go file only contains Go, and editors can highlight the template as Kotlin.
//
// It also avoids two hazards: a backtick in the template rules out Go raw strings, and a %
// has to dodge Fprintf's formatting. A template file doesn't mind either character.
//
//go:embed schema_table.kt.tmpl
var kotlinTemplate string

// tableEntry is one row of the template's data.
type tableEntry struct {
	Key string
	Hex string
}

// RenderKotlin renders the committed Kotlin source.
//
// Hex strings rather than byteArrayOf(...): one entry per line, readable diffs, and the same form
// nervud's logs print schema hashes in (so "the two sides disagree" can be checked by eye).
// Decoding costs once per process, on no hot path.
func RenderKotlin() ([]byte, error) {
	entries, err := Entries()
	if err != nil {
		return nil, err
	}
	// Written sorted by key, decoupled from Entries()' input order (see the notes on Entries for why)
	rows := make([]tableEntry, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, tableEntry{Key: e.Key(), Hex: hex.EncodeToString(e.SchemaHash)})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Key < rows[b].Key })

	tmpl, err := template.New("schema_table").Parse(kotlinTemplate)
	if err != nil {
		return nil, fmt.Errorf("stdinterface: parse kotlin template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Entries []tableEntry }{rows}); err != nil {
		return nil, fmt.Errorf("stdinterface: render kotlin table: %w", err)
	}
	return buf.Bytes(), nil
}
