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

## Against the resume/parallel patch (base `a2fd4e2a`)

`cursor-cache-fields-and-builtin-tools.patch`

Apply last. Two more findings from the same gateway.

**Cache creation was never reported to the client.** `openAIUsagePayload`
carried only `prompt_tokens_details.cached_tokens`, so a cold turn that paid to
fill the cache looked exactly like one that never cached: 42,760 prompt tokens,
18,717 of them read, and nothing said about the 24,041 written. `usage.Detail`
already tracked the figure for the internal statistics; only the wire payload
dropped it. The Anthropic side dropped it as well — `extractOpenAIUsage` read
just `cached_tokens`, so `/v1/messages` clients only ever saw
`cache_read_input_tokens`. Both now carry creation
(`prompt_tokens_details.cache_creation_tokens`, the name the rest of the
repository already parses, and `cache_creation_input_tokens`), and the
Anthropic path takes both counters out of `input_tokens` because Anthropic
reports the three as disjoint. Verify with `tests/cache_fields_probe.py`.

**The workspace built-ins reported a broken environment.** Their diagnostic
came back as Shell with no exit status, Write succeeding but the following Read
failing to find the file, and Glob answering "No exec result". Three separate
causes:

- The workspace is rooted at the process home directory, and the gateway runs
  as a nologin service account whose home does not exist and cannot be created
  by that account. Every `MkdirAll` failed, every error was discarded, and the
  model was handed a path nothing could write to. It falls back to a base that
  works.
- `handleWriteArgs` answered every write with success even when nothing landed,
  which is what made the following read look like the broken step. It answers
  with the real outcome now.
- An exec variant the trimmed proto has no case for decodes to nil, and the
  stream was closed with no result at all. That is what the harness surfaces as
  "No exec result" or a shell with no exit status, and the model retries it for
  whole segments. It throws now, with text the model can act on. The note that
  these built-ins are unavailable was also only added to claude runs carrying
  client tools, so a plain chat never saw it; every run gets it.

Verify with `tests/builtin_tools_probe.py`, which fails if the model reports an
opaque tool failure.

Serving Read from the headless workspace was tried and reverted.
`ReadSuccess_Data` is the reference-image channel, and putting a text file
through it made the provider reject the turn with a 400 on every attempt, so
Read still serves only the reference images attached to the request.

## Against the cache-fields patch (base `d0979e4d`)

`cursor-system-prompt-and-usage-split.patch`

Apply last. The patches above kept treating the usage numbers as basically
right and the tool surface as basically working. Both assumptions were wrong,
and one defect underneath them explains most of what the panel showed.

**The client's system message was thrown away.** It was emitted as a system row
in the prompt blobs, and the harness discards those. Measured on the gateway: an
11,380-token system block produced a prompt of 18,747 tokens, the size of the
harness alone, and the model answered *"I don't have any information about an
access code"* for a secret sent one line earlier. The same content moved into
the user message reported 38,404 tokens and answered correctly. So every caller
that puts context in a system message — which is every agent client — lost it,
was billed as if it had never sent it, and had the largest cacheable block of
its prompt left out of the conversation. That is why so many rows showed no
cache activity at all: there was nothing substantial there to cache.

The `modeb-relay` lineage already knew this. `2d1bc` mirrors its tool_choice
directive onto the user action with the note *"agent scaffolding can drown out
system rows, so repeat the constraint on the user action itself"*. That
workaround was never carried across, and the gateway relied on system rows
alone. Client instructions now travel as a user turn, kept in history order so
they stay inside the cacheable prefix; the short built-in tool constraint is
repeated on the user action, which is always read. After the fix the same probe
reports a 30,324-token prompt and returns the secret.

**The four usage columns double-counted.** The statistics store treats input,
output, cache read and cache creation as disjoint and totals them by addition —
`TotalTokens = input + output + cache read + cache creation` everywhere else in
the repository. The ledger patch put Cursor's `input_tokens`, which is the whole
prompt with the cache counters as subsets, straight into `InputTokens`, so the
cached tokens were counted once at the full input rate and again as cache. The
wire payload keeps OpenAI's inclusive shape; only the statistics detail splits
the cached part back out. This is structurally what `modeb-relay` did, and
reconciles it with the inclusive reading of the raw frames noted above.

**Write and Read disagreed about the workspace.** The previous patch made Write
honest about failures but left it succeeding for files that no reader is
attached to, while Read answered file-not-found — so a run wrote a file, failed
to read it back, and reported the workspace as having lost it. Only generated
images and attached reference images are backed here. Everything else now gets
the same actionable refusal as Shell and Glob, naming the tools that do work,
so the tool surface is at least coherent.

**No Claude-shaped counter may sit beside an inclusive `prompt_tokens`.** OpenAI
has no field for a cache write, so it is tempting to borrow a vendor name that
gateways already read — NewAPI takes `claude_cache_creation_5_m_tokens` as a
sibling of `prompt_tokens`, and emitting it does make the write visible. It also
silently changes how NewAPI reads everything else. Fitting its recorded quota
against the two candidate formulas settles it:

| payload | logged prompt / read / create | quota | inclusive model | disjoint model |
|---|---|---|---|---|
| without the field | 45781 / 24093 / 0 | 24112 | **24112** | 48205 |
| with the field | 45781 / 24093 / 21686 | 75313 | 29534 | **75313** |

`quota = prompt + read×0.1 + create×1.25 + out×5` — the moment a Claude counter
appears, NewAPI treats `prompt_tokens` as Anthropic's disjoint input and charges
the inclusive prompt at the full rate *and* the cache counters on top. That is
the ten-fold cost gap between two otherwise identical rows in the usage panel:
$0.176 against $0.0166 for the same 79,144-token prompt. The field is removed
again; the write still travels inside `prompt_tokens_details`, where it is
unambiguously a subset, and where NewAPI leaves the inclusive reading alone.

Verify with `tests/usage_four_fields_probe.py`, which reconstructs the panel's
four columns from the wire and fails if they do not add up.

### Channel type is the actual fix for Claude clients

An OpenAI-shaped payload cannot report a cache write to NewAPI without breaking
its billing, so Claude models should not take that path at all. A NewAPI channel
of type 1 converts a Claude Messages request to OpenAI and back, which copies
the inclusive `prompt_tokens` into Anthropic's `input_tokens` and leaves the flat
`cache_creation_input_tokens` at zero. Pointing a type 14 (Anthropic) channel at
the same gateway skips the hop entirely, and `/v1/messages` already reports the
four classes disjointly:

```
cold  input_tokens=2  output=3  cache_read=24093  cache_creation=21686  quota=29534
warm  input_tokens=2  output=3  cache_read=45651  cache_creation=128    quota=4742
```

Both rows fit `quota = input + read×0.1 + create×1.25 + out×5` exactly, which is
the correct charge including the cache-write premium.

### grok's estimated cache was manufactured

> Superseded in part by the next section. The reads described here were indeed
> invented, but the conclusion that grok cannot cache was wrong: the frames read
> `0` because the requests were going to the fast tier. The estimator fix below
> stands, and the learning it introduced is what lets grok report a real cache
> once it is off that tier.

grok's usage rows showed cache reads and never a cache write, which looked like
a reporting gap. Every `turn_ended` it produced carried `cache_read_tokens:0`
and `cache_write_tokens:0`, against `cache_read_tokens:18184
cache_write_tokens:8558` for claude on the same harness and the same prompt. The
reads were manufactured here.

A segment that ends at a tool pause has no upstream report to bill from, so the
ledger estimates it, and the estimate counted everything the run had already
sent as a cache read. That is a good guess for a provider that caches and pure
invention for one that does not — a five-turn grok tool loop reported a 95% hit
rate over a cache that was never touched, understating what the run cost. It
also modelled no write, so every estimated segment read from a cache that
nothing in the ledger had ever written to, which is the "only reads, never
creation" shape in the usage panel.

Caching is now learned per model from the reports themselves and remembered for
later runs; until a `turn_ended` shows cache activity, an estimate charges the
whole prompt as input. For a model that does cache, the prefix already sent is
the read and the growth beyond it is the write.

The closing segment needed a cap as well. It receives whatever the estimates got
wrong plus the cache belonging to segments it never served, and was billing a
44,658-token turn with a 56,560-token cache read — a 127% hit rate and a
negative fresh-input figure once downstream subtracted. Cache is a subset of the
prompt that carried it, so it is capped there.

**The steering never named the substitute tool.** It said only that
`mcp_`-prefixed tools work, leaving the model to work out that `mcp_Bash` is the
shell it wanted. It does not: it calls the built-in `Shell`, gets refused, and
spends a segment deciding whether to trust the refusal — which reaches the user
as a transcript full of "this built-in tool is not available" and a model
speculating that its logs are fake. `modeb-relay` had already solved this in
`97bde9d`, enumerating the mapping (`mcp_Bash (use this whenever Bash is
needed)`) and listing the built-ins by name; that wording is restored. Verify
with `tests/colliding_tools_probe.py`, which registers tools called Bash, Write
and Read — shadowing built-ins of the same name — and fails if the model touches
a built-in.

## Against the system-prompt/usage-split patch (base `722cbcf9`)

`cursor-grok-fast-tier-and-cache-shape.patch`

Apply last. It corrects the previous section: grok can cache, and caches very
well. It was never being asked to.

**Every grok request was going to the fast tier.** Cursor's catalog ships grok
with `fast=true` already selected in the default variant, and the wire-id picker
penalised only one direction of the mismatch — wanting fast and not getting it
cost 5 points, while getting a fast variant nobody asked for cost nothing. So
`grok-4.6` resolved to `cursor-grok-4.6-high-fast` for every caller, at score
105. The fast tier is a different serving that answers without prompt caching,
which is why the raw frames showed nothing to report:

```
requested "grok-4.6" -> wire "cursor-grok-4.6-high-fast"
turn_ended:{input_tokens:31217 cache_read_tokens:0 cache_write_tokens:0}
```

A `fast` that came from the catalog default is now dropped rather than treated
as a caller's choice, and an unrequested fast wire id is penalised enough that
the effort bonus cannot select one. Dropping only the parameter — rather than
discarding the whole variant, which was tried first — keeps the rest of the
default, so the reasoning effort survives; discarding the variant silently
demoted `grok-4.6` from `high` to `low`. The `-fast` suffix still reaches the
fast tier, and a model offered only as fast still resolves, with a warning.

Off the fast tier the same probe reads 31,104 of 31,305 prompt tokens from
cache, and the four models checked all resolve to a standard variant:

```
grok-4.6          -> cursor-grok-4.6-high
grok-4.5          -> cursor-grok-4.5-high
claude-sonnet-4-5 -> claude-4.5-sonnet
claude-sonnet-5   -> claude-sonnet-5-thinking-high
```

**grok reports a read and never a write, and that is correct.** xAI reports the
cached portion of the prompt and has no cache-creation counter, because the
uncached remainder is billed as ordinary input rather than at a premium.
Anthropic reports both halves. The ledger held one bit per model — does this
provider cache — which is too coarse: it made an estimate for grok invent a
write, and a cache-creation charge, that nobody levies. The two counters are now
learned separately, and an estimate attributes only the halves its provider has
actually been seen reporting; anything unattributed stays plain input.

Verify with `tests/usage_four_fields_probe.py` or the multi-turn probe below.
Streaming and non-streaming, after the fix:

```
grok-4.6           t1 prompt= 31220 read=  1152 create=     0 fresh= 30068
grok-4.6           t2 prompt= 31356 read= 31104 create=     0 fresh=   252
grok-4.6           t3 prompt= 31427 read= 31232 create=     0 fresh=   195
claude-sonnet-4-5  t1 prompt= 42651 read= 42518 create=   130 fresh=     3
claude-sonnet-4-5  t2 prompt= 42669 read= 42518 create=   148 fresh=     3
```

Through NewAPI the warm turns bill about a quarter of the cold one
(`quota` 8031 against 30452 for the same 31k prompt), and a grok tool loop
reports usage on 6 of 6 segments at a 95% per-turn hit rate.

grok's cache is best-effort with a short TTL, so an occasional cold turn in the
middle of a conversation is upstream behaviour rather than a regression.
