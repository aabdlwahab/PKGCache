// Package eco is the extension point: one interface, one registration site, and a
// descriptor that every other layer derives from.
//
// This exists to fix a specific defect in the previous design. Adding an ecosystem
// there meant editing eight duplicated mapping tables across two codebases — three
// independent copies of an eco→role map, two of a subdir map, plus port maps,
// progress-path maps, a hand-written endpoints block and a TypeScript union. Every
// one of them was a place a seventh ecosystem could be forgotten.
//
// Here an ecosystem is one package implementing Ecosystem plus one line in a
// registry. Routing, the control API, the console's setup instructions, the
// inventory exporter and snapshot inclusion all read Descriptor.
package eco

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/router"
)

// StorageMode says how an ecosystem's content is held.
type StorageMode string

const (
	// StorageBlob keeps content in the content-addressed store, addressed by a
	// catalog entry. Everything except git works this way.
	StorageBlob StorageMode = "blob"
	// StorageManagedDir gives the ecosystem a directory it owns. Git mirrors are
	// live repositories that `git upload-pack` runs against, so they cannot be
	// content-addressed as a unit.
	StorageManagedDir StorageMode = "managed-dir"
)

// ListenerKind says how a request reaches an ecosystem, which is what the project
// router needs in order to work out which tenant a request belongs to.
type ListenerKind string

const (
	// ListenerPathPrefixed is reached at /<project>/<eco>/… — npm, pypi, git, files.
	ListenerPathPrefixed ListenerKind = "path"
	// ListenerProtocolRooted owns a fixed root the client cannot be talked out of.
	// Docker always starts at /v2/, so the project rides the image name instead.
	ListenerProtocolRooted ListenerKind = "protocol-rooted"
	// ListenerForwardProxy receives absolute-form request targets. apt and apk have
	// nowhere in the URL to put a project, so it rides the proxy username.
	ListenerForwardProxy ListenerKind = "forward-proxy"
)

// UpstreamShape describes how an ecosystem's upstreams are configured, so the
// console can render a form for them instead of hard-coding one per ecosystem.
type UpstreamShape string

const (
	// UpstreamSingle is one origin, e.g. npm's registry.
	UpstreamSingle UpstreamShape = "single"
	// UpstreamNamedSet is a map of alias to origin: pypi's indexes, OCI's registry
	// aliases. OCI's set is a floor rather than the whole story — a registry the
	// image name spells out resolves without appearing here at all.
	UpstreamNamedSet UpstreamShape = "named-set"
	// UpstreamNone means the ecosystem derives its origin from the request, as the
	// apt forward proxy and the git mirror do.
	UpstreamNone UpstreamShape = "none"
)

// Freshness classifies a cache key's revalidation policy. Returning this from a
// descriptor is what replaced four bespoke mechanisms — OCI's tag table, git's ref
// table, apt's .meta sidecar files and npm's re-fetch-every-time — with one code
// path in the engine.
type Freshness struct {
	// Immutable content is never revalidated. A wheel, a layer, a .deb.
	Immutable bool
	// TTL is how long a mutable resource is trusted. Zero means revalidate always.
	TTL time.Duration
}

// Immutable is the policy for content addressed by its own digest or version.
var Immutable = Freshness{Immutable: true}

// Revalidate builds a policy for mutable content.
func Revalidate(ttl time.Duration) Freshness { return Freshness{TTL: ttl} }

// SetupStep is one line of copy-paste client configuration.
type SetupStep struct {
	// Comment renders as an explanatory line rather than a command.
	Comment string `json:"comment,omitempty"`
	// Command is a shell line the operator can paste.
	Command string `json:"command,omitempty"`
}

// SetupContext is what the console knows when rendering client instructions.
type SetupContext struct {
	// Host is the cache's client-facing host, without a scheme.
	Host string
	// Port is the client-facing port.
	Port int
	// Project scopes the URLs.
	Project string
	// CAPath is where the operator put the CA certificate.
	CAPath string
	// IsGlobal is true for the default project, which several ecosystems address
	// differently (docker omits the prefix; apt omits the proxy username).
	IsGlobal bool
}

// ClientAuthority joins a client-facing host and port without producing invalid
// URLs for IPv6 literals. Ecosystem setup instructions all use this helper so the
// console cannot advertise six subtly different address formats.
func ClientAuthority(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Descriptor is everything the rest of the system needs to know about an ecosystem.
//
// This is the single declaration site. Nothing else may hold a per-ecosystem table.
type Descriptor struct {
	// ID is the URL segment and the catalog's eco column: "npm", "pypi", "oci", …
	ID string
	// Display is the human name for the console.
	Display string
	// Summary is one line explaining what the ecosystem caches.
	Summary string

	Storage  StorageMode
	Listener ListenerKind

	// Upstreams describes the configuration shape, and Defaults seeds a new install.
	Upstreams        UpstreamShape
	DefaultUpstreams map[string]string

	// Freshness classifies a cache key. Nil means everything is immutable.
	Freshness func(key string) Freshness

	// ParseArtifact derives inventory identity from a cache key. Returning ok=false
	// means the key is cached content but not a semantic artifact — an index page,
	// or an apt by-hash blob.
	ParseArtifact func(key string) (name, version, arch string, ok bool)

	// Setup renders client configuration instructions. This is what previously lived
	// as a hand-written block per ecosystem in the control plane's urls.py, and is
	// why adding an ecosystem needed a frontend change.
	Setup func(SetupContext) []SetupStep
}

// Route is one HTTP route an ecosystem serves, relative to its own root.
type Route struct {
	Methods []string
	// Pattern uses the router's syntax and is relative to the ecosystem root.
	Pattern string
	Handler router.Handler
	// Admin marks an operational route ("+indexes", "+maintain") rather than a
	// client-protocol one. Admin routes register first so a greedy protocol
	// catch-all cannot shadow them.
	Admin bool
}

// Ecosystem is the contract. Two methods: what you are, and what you serve.
type Ecosystem interface {
	Descriptor() Descriptor
	Routes() []Route
}

// Registry holds the registered ecosystems.
//
// A value type rather than a package global, so a test can build one with a single
// fake ecosystem and the composition root stays the only place the real set is
// assembled.
type Registry struct {
	byID  map[string]Ecosystem
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Ecosystem)}
}

// Register adds an ecosystem. Registering the same ID twice is a programming error
// and panics at startup rather than silently shadowing.
func (r *Registry) Register(e Ecosystem) {
	d := e.Descriptor()
	if err := validate(d); err != nil {
		panic(fmt.Sprintf("eco: %v", err))
	}
	if _, dup := r.byID[d.ID]; dup {
		panic(fmt.Sprintf("eco: %q is registered twice", d.ID))
	}
	r.byID[d.ID] = e
	r.order = append(r.order, d.ID)
	sort.Strings(r.order)
}

// Get returns an ecosystem by ID.
func (r *Registry) Get(id string) (Ecosystem, bool) {
	e, ok := r.byID[id]
	return e, ok
}

// IDs returns every registered ID, sorted, so API output and route registration are
// deterministic.
func (r *Registry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// All returns every ecosystem in ID order.
func (r *Registry) All() []Ecosystem {
	out := make([]Ecosystem, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Descriptors returns every descriptor in ID order. This is what
// GET /api/v1/ecosystems serves, and what lets the console render a new ecosystem
// with no frontend change.
func (r *Registry) Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id].Descriptor())
	}
	return out
}

// Len reports how many ecosystems are registered.
func (r *Registry) Len() int { return len(r.byID) }

// FreshnessFor classifies a key, defaulting to immutable when the ecosystem declares
// no policy.
func (d Descriptor) FreshnessFor(key string) Freshness {
	if d.Freshness == nil {
		return Immutable
	}
	return d.Freshness(key)
}

// Artifact derives inventory identity from a key, or reports that the key is not an
// artifact.
func (d Descriptor) Artifact(key string) (name, version, arch string, ok bool) {
	if d.ParseArtifact == nil {
		return "", "", "", false
	}
	return d.ParseArtifact(key)
}

// SetupSteps renders client instructions, or nothing when the ecosystem declares none.
func (d Descriptor) SetupSteps(ctx SetupContext) []SetupStep {
	if d.Setup == nil {
		return nil
	}
	return d.Setup(ctx)
}

func validate(d Descriptor) error {
	if d.ID == "" {
		return fmt.Errorf("descriptor has no ID")
	}
	for _, c := range d.ID {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("ID %q must be lowercase alphanumeric or dash: it is a URL "+
				"segment and a catalog column value", d.ID)
		}
	}
	switch d.Storage {
	case StorageBlob, StorageManagedDir:
	default:
		return fmt.Errorf("ecosystem %q has an unknown storage mode %q", d.ID, d.Storage)
	}
	switch d.Listener {
	case ListenerPathPrefixed, ListenerProtocolRooted, ListenerForwardProxy:
	default:
		return fmt.Errorf("ecosystem %q has an unknown listener kind %q", d.ID, d.Listener)
	}
	switch d.Upstreams {
	case UpstreamSingle, UpstreamNamedSet, UpstreamNone:
	default:
		return fmt.Errorf("ecosystem %q has an unknown upstream shape %q", d.ID, d.Upstreams)
	}
	return nil
}
