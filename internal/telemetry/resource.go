package telemetry

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// buildResource assembles the attributes attached to every signal this process
// emits, once, at startup.
//
// The semantic-convention names are the point of using OpenTelemetry at all
// (ADR §31 D1): `service.version` means the same thing arriving from Toft
// as it does arriving from the service beside it in the operator's rack, and
// that is worth more than any local naming taste.
func buildResource(ctx context.Context, opts Options) (*resource.Resource, error) {
	host, _ := os.Hostname()

	attrs := []attribute.KeyValue{
		semconv.ServiceName(opts.Env.ServiceName),
		semconv.ServiceVersion(opts.Version),
		semconv.ServiceInstanceID(instanceID(opts, host)),
		semconv.HostName(host),
		// GOOS is already the vocabulary os.type uses ("linux", "darwin",
		// "windows"), so the key takes it directly rather than through the
		// enum helpers, which cover a subset of the platforms Go builds for.
		semconv.OSTypeKey.String(goruntime.GOOS),
		semconv.ProcessRuntimeName("go"),
		semconv.ProcessRuntimeVersion(goruntime.Version()),
	}
	if ns := opts.Env.ServiceNamespace; ns != "" {
		attrs = append(attrs, semconv.ServiceNamespace(ns))
	}
	if opts.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(opts.Environment))
	}
	// Omitted when empty rather than stamped with a placeholder: a build with no
	// VCS stamp (`go run`, a tarball, a vendor rebuild) is a normal build, and
	// internal/version says so in its own package doc.
	if opts.Revision != "" {
		attrs = append(attrs, attribute.String("toft.revision", opts.Revision))
	}
	if opts.ExecutionMode != "" {
		attrs = append(attrs, attribute.String("toft.execution_mode", opts.ExecutionMode))
	}

	base := resource.NewWithAttributes(semconv.SchemaURL, attrs...)

	// OTEL_RESOURCE_ATTRIBUTES merges on top, so an operator can add team= or
	// region= without a code change. On top, not underneath: it is theirs, and
	// the platform's own guess at service.name should lose to an explicit one.
	env, err := resource.New(ctx, resource.WithFromEnv())
	if err != nil {
		// A malformed OTEL_RESOURCE_ATTRIBUTES is the operator's typo, and the
		// detector reports it while still returning what it could parse. Worth
		// a line; not worth refusing to export.
		fmt.Fprintf(os.Stderr, "telemetry: OTEL_RESOURCE_ATTRIBUTES: %v\n", err)
	}
	if env == nil {
		return base, nil
	}
	merged, err := resource.Merge(base, env)
	if err != nil {
		// Merge only fails on a schema-URL conflict. Keeping the platform's own
		// resource beats having none.
		return base, nil
	}
	return merged, nil
}

// startedAt is stamped once so service.instance.id is stable for the life of
// the process — a value that changed per export would make every counter look
// like it reset.
var startedAt = time.Now().UTC()

// instanceID identifies this process among the install's replicas.
//
// Hostname plus start time, which is right for both shapes this runs in: on a
// VM the hostname is stable and the timestamp distinguishes restarts, and in
// the cluster the pod name is already the hostname. OTEL_SERVICE_INSTANCE_ID
// overrides it for a topology neither of those describes.
func instanceID(opts Options, host string) string {
	if id := opts.Env.ServiceInstanceID; id != "" {
		return id
	}
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s-%d", host, startedAt.Unix())
}
