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
conversation — the probe does this with a nonce — because an exact duplicate of
an earlier prompt resumes that conversation from the checkpoint cache instead
of starting a new turn, and answers the resumed context rather than the
request.
