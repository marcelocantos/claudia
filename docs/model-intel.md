# Model intel (purpose-quality series)

Status: MVP for 🎯T71 (2026-09-14).

Resolve can pick a **purpose-quality floor** and return **generation +
effort**. Those two stay separate in the store because the boards
publish them separately; the host does not specify them in the normal
case.

## Predicates

| You pass | Resolve does |
|---|---|
| `Purpose` + `Quality` | Search the `(generation, effort)` grid; emit both |
| `Model` and/or `Effort` | Pin; fail closed if a quality floor is set and missed |
| neither Purpose nor pins | Catalog-shelf path (🎯T61.3) — unchanged |

Empty `Quality` means `standard`. Empty `Purpose` is **not** `general`
on the catalog path — set `ModelPurposeGeneral` when the job is broad.
A requested purpose with no catalog-overlapping observations yields to
`general` (the usual gap is `analysis` / HLE). A series that exists but
misses the floor still fails closed.

Available-tokens still veto weekly-hot, session-low, and exhausted.
Among the rest, lower plan pressure (blue/purple slack) ranks first —
on the catalog path too, not only when a purpose series is present.
`PreferProvider` only breaks a slack tie. A remaining tie fails closed
(the catalog is a set, not a ranking). Research cost is the next key
on the intel path only.

## Store

Append-only JSONL under `$CLAUDIA_MODEL_INTEL` or
`StateDir/model-intel/`:

- `observations.jsonl` — one score (and optional `$` cost) per row
- `ingest_runs.jsonl` — success or skip/error; missing AA key does not
  invent numbers

Identity is `(generation, effort, purpose, source)`. A second ingest is
a new reading. `DriftModelIntel` compares the last two: a `source_rev`
change is a board event, not a model move.

## Sources (MVP)

Artificial Analysis free catalog
(`GET /api/v2/language/models/free`, `CLAUDIA_AA_API_KEY`):

| AA field | Purpose |
|---|---|
| `artificial_analysis_intelligence_index` | `general` |
| `artificial_analysis_coding_index` | `coding` |
| `artificial_analysis_agentic_index` | `agent` |
| `artificial_analysis_math_index` or `hle` | `analysis` |
| `aa_omniscience_accuracy` | `browse` |

Cost is AA's `$` per Intelligence Index task when published. Arena,
METR, and Epoch are not ingested in this slice.

Quality floors are percentiles of that day's catalog-overlapping scores
for the purpose: economy = min, standard = p40, frontier = p75.

## CLI

```bash
claudia models intel refresh
claudia models intel latest --purpose coding
claudia models intel history --generation claude-opus-5 --effort high --purpose coding
claudia models intel drift
```

The daemon calls refresh at most once per day. No key → skip, keep
yesterday.

## Oracles

```bash
go test ./... -count=1 -run 'TestParseModelSlug|TestRefreshModelIntel|TestDriftModelIntel|TestResolvePurpose|TestResolveRedYields|TestResolvePinMisses|TestResolveFallsBack|TestResolveDoesNotFallBack|TestResolveCatalogPath'
```
