# Opt-in container lifecycle controls

`TestContainerLifecycleRuntime` skips by default. It uses a verified local immutable BusyBox-compatible image, real SOAP tasks, rootless Podman, private networks and an exact-owner delegating command interceptor. It never pulls an image or runs an appliance.

Set these variables explicitly before running:

- `VCSIM_LIFECYCLE_RUNTIME=1`
- `VCSIM_LIFECYCLE_IMAGE=sha256:<existing-local-image-id>`
- `VCSIM_LIFECYCLE_ENGINE=/absolute/path/to/podman`
- `VCSIM_LIFECYCLE_ENGINE_SHA256=<expected-engine-binary-sha256>` (optional additional fence)
- `VCSIM_LIFECYCLE_GRAPH_ROOT=/absolute/expected/graph/root`
- `VCSIM_LIFECYCLE_ARTIFACTS=/absolute/artifact/directory`

Run `go test -p 1 ./simulator -run '^TestContainerLifecycleRuntime$' -v -count=1 -timeout=4m` under the host's normal simulator runtime lock and CPU limits. A clean simulator host is required: the preflight refuses any existing run-labelled container before invoking the production stale-container sweep. Python 3.9+ is needed for the interceptor.

The controls cover failed start followed by public Destroy error/retry, HTTP client and task-status cancellation during real creation, normal deletion, and network/volume removal failure after the container is already gone. Owned anonymous/named volumes, networks and exposed sandbox paths must disappear. Separately owned live-run sentinels must survive until fixture teardown. An empty sandbox path is reported as unknown rather than absence.

Artifacts retain owner UUID/run/PID/start, immutable image and engine identities, exact resource IDs and anonymous-volume creation identity, command/fault records, API wall times, natural outcomes and pre-rescue state. Fixture cleanup releases barriers and clears faults before waiting for server ownership, then validates every exact owned identity before rescue. It continues other cleanup after an error and reports functional failure independently from final residue. Only a completed owner wait or typed `ManagedObjectNotFound` resolves server ownership. Deadline, transport and other wait errors remain `ownershipUnknown`: even an empty exact-resource census cannot establish complete cleanup while later server creation remains possible.

For a deliberate fixture-cleanup negative control, set `VCSIM_LIFECYCLE_ABORT_DURING_CREATE=1` and select only `TestContainerLifecycleRuntime/client-cancel`. This covers assertion failure at that known-create barrier only. The test must fail there while cleanup still removes every owned resource; do not treat that expected test failure as a product regression. The caller should retain an independent exact-ID final census, especially when terminating a test process externally. Do not use broad pruning, process-name killing or guessed namespace unlinking.
