# Patch series

Patches to apply to the upstream CPA repository (`Apologetic2022/cpa`) with
`git am`. They do not all share a base, so check the base before applying.

## Series against `modeb-relay` (base `41763d7`)

`0001`–`0007`, plus `cursor-toolcall-cache-fix-combined.diff`, which is the
same work as one combined diff. This is the lineage vendored into
`cpa-modeb-relay/` in this repository.

## Against `cursor/restore-cursor-prompt-cache-1e92` (base `8cb3c3f3`)

`cursor-toolloop-usage-ledger.patch`

The two lineages both fix prompt caching, but each fixes a different half, and
until this patch neither branch had both:

- `cursor/restore-cursor-prompt-cache-1e92` makes the upstream cache actually
  *hit*: conversation checkpoint reuse, request-prefix anchoring, and tool-loop
  resume, so a continuation replays under the same Cursor conversation instead
  of re-billing the whole prefix.
- The `modeb-relay` series makes the resulting usage *reportable*. Cursor emits
  usage once per Agent run, in the TurnEnded that closes it, with counters
  cumulative over every segment. One gateway request drives one segment, so
  every request that ends at a tool-call pause gets no usage at all, and the
  request that closes the run reports the entire run again.

`cursor-toolloop-usage-ledger.patch` ports the second half onto the first. It
is what makes an agent tool loop stop showing five-figure prompts with no
cache: measured on the production gateway, 66 of 103 requests answered with no
usage anywhere in the response before it, and none after.

It also corrects how the two are combined. The `modeb-relay` series reads
Cursor's counters as disjoint, the way the Anthropic field names suggest, and
sums them into the prompt total. Raw `turn_ended` frames say otherwise:
`input_tokens:18817 cache_read_tokens:18717 cache_write_tokens:98` is an ~18.8k
prompt served almost entirely from cache, not a 37.6k one. This patch keeps
`input_tokens` as the whole prompt with the cache counters as subsets.

Verify with `tests/toolloop_strict_probe.py`, which drives a forced multi-turn
tool loop and prints the per-turn cache share. Give every run a distinct
conversation — the probe does this with a nonce — so each run is measured as a
fresh turn rather than a continuation of the last one.

## Against the ledger patch (base `9e77c37a`)

`cursor-resume-and-parallel-tools.patch`

Apply after `cursor-toolloop-usage-ledger.patch`. Two defects, both found in
production traffic on 15.204.94.214, both visible in the monitor's token
column as rows that create cache but never read any.

**A repeated request answered the wrong conversation.** `lookupPrefixResume`
probes every message boundary for a checkpoint, longest prefix first. When the
checkpoint covered the request end to end there was no tail left to send, so
the resume carried the literal text `Continue.` instead. That is not a
continuation — it is a retry, a polling monitor, or two agents opening with the
same turn — and the model answered the finished conversation: *"I don't have
any task to continue."* The replay also rewrote the prompt, so the upstream
cache missed and the turn was billed as a fresh cache write, which is the
create-without-read the monitor showed. Sending one prompt six times reproduced
it exactly: only the first answered correctly, and the prompt grew every turn
as the nudge and its wrong answer were appended.

A boundary that folds to an empty tail is now skipped instead of resumed. The
probe keeps walking, so a shorter boundary carrying a real instruction still
resumes and still hits the cache, while a fully covered request runs fresh.
Verify with `tests/repeat_request_probe.py`: all six repeats answer `38` at a
flat 18,775-token prompt with a ~99.5% cache read.

**A parallel tool batch arrived one call per round trip.** The read loop parked
the segment on the first client tool call it decoded. Upstream emits a batch as
back-to-back frames spanning about 13ms, which often but not always land in one
read of the response body, so the rest of the batch slipped into later turns.
Body reads move to a dedicated goroutine feeding a channel, letting the
consumer bound its wait, and the loop drains for `parallelToolGrace` after the
first tool call before parking. The mid-turn checkpoint, pending-usage flush
and pause bookkeeping still run once, after the drain.

Verify with `tests/parallel_toolcall_probe.py` on `grok-4.6` or
`claude-opus-5`; `claude-sonnet-4-5` and `claude-fable-5` are serialised
upstream by the agent harness and answer one call at a time regardless.
Non-streaming grok-4.6 went from 7/8 full batches to 8/8.
