# Timing and profiles

vcsim answers requests as fast as it can unless told otherwise. It has no timing
profile file and no profile flag. Timing is controlled by three kinds of setting:

1. Command-line flags that delay SOAP API responses and task bodies.
2. Two environment variables that tune how container-backed VMs are monitored.
3. Package variables in the Go API, for everything else.

Everything defaults to no added delay, except the disk promotion sleep described
below, which is on by default.

The timing knobs change how long vcsim takes. They do not change what it
returns. For how container VMs and networks behave, see
[container-vms-and-networking.md](container-vms-and-networking.md).

## Command-line delay flags

| Flag | Default | Applies to | Effect |
| --- | --- | --- | --- |
| `-delay <ms>` | `0` | Every SOAP method call | Sleeps this many milliseconds before the handler runs. |
| `-delay-jitter <cv>` | `0` | SOAP method calls | Randomises each call's delay with a normal draw. `<cv>` is the coefficient of variation: the standard deviation as a fraction of the mean. `0.5` is a reasonable start. |
| `-method-delay <list>` | none | Named methods and named tasks | Adds a per-name delay, as `name:ms,name:ms`. |

A call's delay is the sum of its parts:

```text
delay = -delay + (-method-delay entry for the call's name, if any)
delay = delay + normal(0, 1) * cv * delay        when -delay-jitter cv > 0
```

- A delay of zero or less means no sleep. A jittered draw that comes out
  non-positive therefore means no sleep, so jitter skews the observed
  distribution at low delays.
- The sleep happens before the handler runs.
- Injected faults are returned before the delay. A fault that should be slow
  needs its own `Delay` on the fault rule (see `simulator/doc.go`).
- `-delay` and `-delay-jitter` apply to every SOAP call, including reads, login
  and the polling calls a client makes while it waits for a task. A client that
  polls is slowed on each poll, so its cadence changes too.

### Naming methods and tasks

`-method-delay` has two kinds of key, and both are matched exactly, with case.

- **SOAP method names** delay the API call that starts the method. For example,
  `PowerOnVM_Task` delays the response to the `PowerOnVM_Task` request, which
  returns the task.
- **Task names** delay the task's body, which runs after the request returns. A
  task's name is its runner's name with the first letter capitalised, and it has
  no `_Task` suffix. For example, the power-on task is `PowerOn`, and a destroy
  task is `Destroy`.

Both kinds of key come from the same flag. A key that matches both a method and
a task adds to both. For example, `PowerOnVM_Task:500,PowerOn:1500` makes a
power-on request take 500 ms to answer, and then its task takes 1500 ms more to
finish.

The list syntax is strict. Each entry must be `name:ms`, with exactly one colon,
and `ms` must be an integer. An empty entry, such as a trailing comma, is an
error. Whitespace around entries is ignored.

A key named `LockHandoff` is not a delay. A value of `0` turns off the handoff of
the object lock to the task goroutine (see `simulator/task.go`). It changes
locking, not timing. Treat it as internal.

### Examples

Baseline latency for a client on a network with about 40 ms round trip time:

```sh
vcsim -l 127.0.0.1:8989 -delay 40 -delay-jitter 0.3
```

Slow power operations, with everything else at default speed:

```sh
vcsim -l 127.0.0.1:8989 \
  -method-delay 'PowerOnVM_Task:300,PowerOn:1500,PowerOffVM_Task:200'
```

Check the task names in `simulator/` before relying on one. `PowerOn` is the
name the power-on runner registers. Others follow the same rule.

## Container monitoring environment variables

These affect container-backed VMs only. They are read once, when the vcsim
process starts. An unparsable value is ignored, and the default applies. Values
are Go durations, such as `250ms` or `2s`.

| Variable | Default | Effect |
| --- | --- | --- |
| `VCSIM_EVENT_CONSOLIDATION_WINDOW` | `250ms` | After a container event, vcsim waits this long so a burst of changes is read in one inspection. |
| `VCSIM_INSPECT_INTERVAL` | `10s` | How often a watched container is inspected even without an event. This is a backstop, so a missed event delays a power-state change by up to this long. |

## Go API timing variables

These package variables are for test code that uses the `simulator` package
directly. None of them has a command-line flag. Set them before creating a model
or serving requests.

| Variable | Default | Effect |
| --- | --- | --- |
| `Model.DelayConfig` | zero | The same delays as the flags, set in Go: `Delay`, `MethodDelay`, `DelayJitter`. |
| `simulator.TaskDelay` | zero | Delays for task bodies, with the same fields. The flags set only `MethodDelay` on it. |
| `PromoteDisksPerGiB` | 1024 ms | Sleep per GiB of disk in `PromoteDisks_Task`. The default is about one second per GiB. `0` disables it. |
| `PromoteDisksJitter` | `0` | Jitter of that sleep, as `-delay-jitter`. |
| `ContainerIPAttempts` | `5` | How many times vcsim polls a started container for an IP. |
| `ContainerIPInterval` | `500ms` | Time between those polls. |
| `PropertyCollectorBatchWindow` | `20ms` | How long `WaitForUpdates` waits to gather updates before it replies. |
| `TaskWaitPoll` | `10ms` | How often `Task.Wait` checks for completion. |
| `PodmanHelperStartPoll`, `PodmanHelperStartTimeout`, `PodmanHelperStopTimeout` | `20ms`, `10s`, `5s` | Timing of a router's network namespace helper. |

`PromoteDisks_Task` sleeps by default, so a test that promotes disks is slower
than a flag-less run suggests. Set `PromoteDisksPerGiB` to `0` for the fastest
behaviour.

### Reproducible jitter

Jitter is random. The flags draw from the global random source, so two runs with
the same flags get different delays. To make jitter reproducible in Go, call
`simulator.SeedTiming(seed)`. The same seed gives the same sequence of draws. When
several requests draw concurrently, the draws are shared, so which request gets
which draw depends on scheduling.

## How the knobs interact with container VM boot

A container-backed VM's power-on has three parts with different timing.

- **The API part** is `PowerOnVM_Task`, with the `-delay` and `-method-delay`
  entries for its name, and jitter.
- **The task part** is the power-on task, with the task-name entry for `PowerOn`.
  Inside it, vcsim creates the container, starts it, and polls it for an IP for up
  to `ContainerIPAttempts` times `ContainerIPInterval` (2.5 seconds by default),
  then starts watching it. The engine's own work (image pull if the image is
  missing, container create and start) is real wall time, and no knob governs it.
- **The guest part** is the container's process. vcsim does not simulate a guest
  boot sequence. A guest that takes time to become ready does so because its
  command takes time, for example a command that sleeps before its service starts.
  Use the image's command for this, or make the harness wait for readiness.

Power off and reset are real container operations (`docker stop`, `docker
restart`). Their time comes from the engine. vcsim does not pass a stop timeout
or stop signal, so an image whose PID 1 ignores SIGTERM takes the engine's default
stop timeout, typically 10 seconds, before the container is killed. This is engine
behaviour, not something vcsim sets.

Pause (suspend) is `docker pause`.

## Realistic latency recipe

The flags add delay. They do not model a network. The steps below calibrate
vcsim's responses to an endpoint you have measured.

1. **Measure the real endpoint.** Time representative calls against the real
   vCenter from the client's location: a property read, a `PowerOnVM_Task` request,
   and the time from request to task completion. Keep the median and the spread.
2. **Set the base delay.** Set `-delay` to the round trip time to the endpoint,
   which is the cost every call pays. A value of 20 to 50 ms is typical for a
   regional link.
3. **Set jitter.** `-delay-jitter 0.3` to `0.5` gives a spread similar to a shared
   link. Start low and raise it only if the client's retry logic needs exercising.
4. **Add slow methods.** For a method slower than the base, add a `-method-delay`
   entry for the extra time. The entries add to `-delay`, so subtract the base from
   the measured figure. A measured 900 ms `PowerOnVM_Task` with a 40 ms base needs
   `PowerOnVM_Task:860`.
5. **Add task time.** Task durations are a separate figure. Add a task-name entry
   for the task's body, measured from request to completion minus the request's own
   time.
6. **Check the result.** Count the calls and time a few operations. Check that a
   polling loop still finishes within its own timeout. Check that jitter did not
   turn a timeout into a flake.

Limits of the recipe:

- Delays are per call. They do not depend on message size, connection reuse,
  TLS handshakes or the number of concurrent connections.
- Container traffic has no latency, loss or bandwidth model. Traffic between
  container VMs runs at the engine's speed.
- Task scheduling on a real vCenter is not modelled. A task's time is its
  configured delay plus its real work in vcsim.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `Incorrect method delay format.` | An entry has no colon, or is empty, such as a trailing comma. |
| `Incorrect format of method-delay argument` | An entry's delay is not an integer, such as `500ms`. Use a plain number of milliseconds. |
| Power-on slower than the API delays explain | Container start, image pull, or the IP poll (up to 2.5 s by default). |
| Power-off takes about 10 seconds | The container's stop timeout. See above. |
| Jitter seems to have no effect | `-delay` is zero and there is no `-method-delay` entry for the call, so there is nothing to jitter. |
| Task delay did not apply | The key must match the task name, such as `PowerOn`, not `PowerOnVM_Task`. |
