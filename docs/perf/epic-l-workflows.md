# EPIC L scale measurements — scripted backup workflows

The seven scale scenarios issue #812 asks for, as benchmarks in the tree
rather than numbers somebody once saw. A published cost that cannot be
re-run is a claim: nobody can check it, and nobody notices when a change
makes it wrong. This file records the commands, the machine and what each
row means; `core/internal/workflowrun/gate_bench_test.go` is what those
commands run.

This is a **baseline to compare against, not a threshold this gate
enforces**. `docs/perf/gate.json` is the Phase 6 regression gate and names
one designated host and one workload; adding wall-clock rows from a laptop
to it would turn a loaded machine into a red build. What #812 actually
requires to be enforced is asserted deterministically instead, by tests
rather than by timings — see "The claims that are gated" below.

## Running it

```
cd core && go test ./internal/workflowrun/ -run '^$' -bench . -benchtime 1x -count 5
```

`-benchtime 1x` because each iteration stages its own fixture: a real
SQLite journal, a real workflow tree, and for the hundred-hook rows a
hundred scripts read, hashed, custody-checked and copied into a spool. A
larger iteration count would mostly measure the filesystem building the
fixture. `-count 5` because wall clock on a laptop varies by a factor of
two between runs; what is worth reading is the RATIO between neighbouring
rows and the allocation figures, which are deterministic.

## The machine

```
Darwin 25.6.0, arm64, Apple M5, 10 cores
go 1.27.1
```

Not `docs/perf/baselines/darwin-arm64-mac17-2.json`'s designated host
record, and deliberately not added to it: these rows are a different
workload and would have to be captured there before they could be
compared there.

## What was measured

| Scenario | Row | ns/op | Other metrics |
|---|---|---|---|
| Workflows disabled | `BenchmarkAPlanWithNoWorkflowConfiguredAtAll` | 24,708 | 576 B/op, 5 allocs/op, 4.41 heap_mb |
| — the same backup, no engine at all | `BenchmarkBackupWithoutTheEngine` (pre-existing) | 42 | 0 B/op |
| — the same path, pre-existing baseline row | `BenchmarkZeroHookRun` (pre-existing) | 1,791 | 576 B/op, 5 allocs/op |
| Stage directories exist and are empty | `BenchmarkAPlanWhoseStageDirectoriesAreEmpty` | 15,436,917 | 89,528 B/op, 1,304 allocs/op |
| 100 tiny local hooks | `BenchmarkAHundredTinyLocalHooks` | 446,650,667 | 400.5 planning_ms, 6.58 heap_mb, 29,548 allocs/op |
| 100 remote hooks | `BenchmarkAHundredRemoteHooks` | 437,810,542 | 408.0 planning_ms, 6.59 heap_mb, 29,657 allocs/op |
| A high-output script | `BenchmarkAHighOutputScript` | 15,422,708 | 256.2 persisted_kib, 60,201 ns/KiB, 5.64 heap_mb |
| A 100-step historical run, replayed | `BenchmarkLogsAfterOverAHundredStepHistory` | 101,667 | 100 records, 76,760 B/op |
| A slow follower while a script emits | `BenchmarkASlowFollowerWhileAScriptEmits` | 35,892,125 | 26.30 hook_ms, 398 dropped, 4.85 heap_mb |

Four things in that table are worth stating in words, because a number on
its own invites the wrong conclusion.

**Planning is the cost of a hundred hooks, not execution.** 400 ms of the
447 ms local row is `workflow.Snapshot` — roughly 94%, about 4 ms per
script. That is one read, one sha256, one custody walk from the script up
to the filesystem root, and one copy into the run's spool, per script, and
it is the price of the guarantee that a resumed cleanup executes the bytes
the run captured rather than whatever is in `/workflows` now. It is also
where to look first if a deployment with many hooks ever feels slow: the
execution loop is the cheap half.

**A hundred hooks is 25 in each of the four stages.** `MaxScriptsPerStage`
is 64 and the whole directory is refused past it, so "a hundred scripts"
cannot mean one directory. The benchmark says so in its own comment.

**Empty stage directories cost four directory resolutions and a plan, not
nothing.** The 15 ms row is dominated by the custody walk each configured
stage does; a deployment that configures no root at all takes the 24 µs
row instead, which is the number the "no behaviour change" promise is
about.

**Runner RSS is deliberately absent.** A local hook runs in an ephemeral
container started by a separate host process (#865), and a remote hook
runs on another machine, so this process cannot honestly report either
one's resident set. What is reported is the engine's own heap
(`runtime.MemStats.HeapAlloc`, as `heap_mb`). The runner's real behaviour
is `core/tests/containerhooks`' question, against a real daemon.

## The claims that are gated

#812's acceptance criteria are phrased as performance claims, and three of
them are assertable exactly rather than approximately. Those are tests,
not benchmarks, because a timing assertion on a shared runner fails for
reasons that have nothing to do with the code:

- **No meaningful regression on the no-hooks path** →
  `TestTheNoHookPathDoesNoJournalWorkAtAll` counts every call a run makes
  to the durable store and requires it to be zero for a set with no
  workflow configuration, and `TestTheZeroHookPathRecordsNothing` requires
  no run row to be written. "Does no work" is a stronger and steadier
  claim than "takes about as long".
- **Log limits bound persisted storage** →
  `TestALogFloodIsBoundedAcrossAWholeRun` holds the whole run's persisted
  payload to the per-step bound times the number of steps, and
  `TestPersistedOutputIsBoundedWithATruncationMarker` puts the marker in
  sequence position.
- **A slow or disconnected client does not extend a script's duration** →
  `TestAStalledFollowerDoesNotSlowTheHookAndCatchesUpByCursor` compares the
  hook's own wall clock against a no-follower control and then proves the
  catch-up read is gapless. The benchmark row above reports the same thing
  as numbers (`hook_ms`, `dropped`) so a change in the drop behaviour is
  visible, but the gate is the test.

`docs/conformance/epic-l-matrix.md` row GC-16 is where these are tracked
with their falsifications.
