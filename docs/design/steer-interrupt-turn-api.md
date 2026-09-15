# Steer and interrupt — Claudia turn-delivery API

Status: **design (2026-09-15)** — owner-approved direction from jevons overseer session.

## Problem

Today `Agent.Send` conflates three owner intents:

1. **Submit** — start a turn when idle.
2. **Queue** — hold text until the current turn finishes.
3. **Interrupt** — hard-cancel the open turn, then submit.

Cursor (and some ACP agents) also support **steer**: fold new user text into the
*running* turn without waiting for idle and without tearing down in-flight work
the way `session/cancel` does. Claudia currently **rejects** a second
`session/prompt` (`prompt already in flight`), so hosts build client-side sendq +
uncertain delivery instead of using the agent's native steer.

Jevons composer keys also invert the desired mapping (Cmd+Enter = interrupt today).

## Goals

- Expose **Steer** and **Interrupt** as first-class Claudia API verbs (library +
  broker wire), with honest capability reporting per provider.
- Keep **Queue** as a **host-side** intent (composer + jevons UI), not an ACP wire
  primitive — but the broker may accept `mode=queue` for symmetry and logging.
- Map each backend to the best available mechanism; never pretend queue-until-idle
  is steer.
- Jevons PO / fleet seats consume the same modes through convomux + MCP.

Non-goals (this design):

- ACP v2 `session/inject` — track separately; adapters may grow into it.
- Fleet-agent sendq PIN/uncertain semantics — shrink once steer works; separate
  target for migration.

---

## Owner intents (product semantics)

| Intent | Owner action (jevons composer) | When turn idle | When turn in flight |
|--------|--------------------------------|----------------|---------------------|
| **Submit** | Enter (plain) | Send now | **Enqueue** (FIFO) |
| **Steer** | ⌘Enter | Send now (same as submit) | Steer with text |
| **Interrupt** | ⌘⇧Enter | Send now (same as submit) | Cancel turn, then send |
| **Queue** | (implicit on submit while busy) | — | Hold until turn boundary |

**Composer queue UX (jevons):**

- Queued messages render **above** the composer (newest at bottom of queue strip,
  oldest at top — or vice versa; pick one and test; default: **append = bottom**,
  display list **bottom = next to send**).
- **Alt+↑ / Alt+↓** cycle **queue focus** first; only when queue is empty do those
  keys fall through to **transcript rewind recall** (today's Alt+↑ history).
- With a queue item focused, **⌘Enter** steers that text; **⌘⇧Enter** interrupts
  with that text (removes item from queue on success).
- Plain **Enter** on empty composer while busy: noop (do not steal focus).

---

## Claudia public API

### Types

```go
// TurnPhase is observed seat state (not a delivery outcome).
type TurnPhase string

const (
    TurnIdle    TurnPhase = "idle"
    TurnInTurn  TurnPhase = "in_turn"
)

// SteerPolicy describes how a provider absorbs steer when supported.
type SteerPolicy string

const (
    SteerBreakpoint    SteerPolicy = "breakpoint"     // e.g. Cursor ACP
    SteerFinishSlice   SteerPolicy = "finish_slice"   // e.g. Grok ACP
    SteerQueueUntilIdle SteerPolicy = "queue_until_idle" // honest fallback
    SteerNone          SteerPolicy = "none"
)

type TurnCaps struct {
    CanInterrupt bool
    CanSteer     bool
    SteerPolicy  SteerPolicy
    // BusyOnSecondSubmit: what the provider does if a second submit arrives
    // while in_turn without an explicit Steer call.
    BusyOnSecondSubmit string // "reject" | "supersede" | "queue" (observed)
}

// DeliveryMode selects the host intent for one user-text delivery.
type DeliveryMode string

const (
    DeliverySubmit    DeliveryMode = "submit"    // default; queue if busy (host)
    DeliverySteer     DeliveryMode = "steer"
    DeliveryInterrupt DeliveryMode = "interrupt"
    DeliveryQueue     DeliveryMode = "queue"     // explicit enqueue; no wire send
)

// DeliveryOutcome is returned synchronously from Steer/Interrupt/Submit paths.
type DeliveryOutcome struct {
    Mode           DeliveryMode
    PhaseBefore    TurnPhase
    // Mechanism records what actually ran (for logs/UI); not a second truth.
    Mechanism      string // e.g. "acp_session_prompt_supersede", "session_cancel+prompt", "client_queue"
    SupersededTurnID string // when steer superseded an in-flight prompt RPC
    Err            error
}
```

### Agent methods

```go
// TurnPhase / TurnCaps — read-only introspection.
func (a *Agent) TurnPhase() TurnPhase
func (a *Agent) TurnCaps() TurnCaps

// SendMode delivers text with an explicit mode. Send(text) == SendMode(text, DeliverySubmit)
// with host queue-when-busy behaviour preserved for backward compatibility until migrated.
func (a *Agent) SendMode(text string, mode DeliveryMode) (DeliveryOutcome, error)

// Steer folds text into the running turn when supported; error if idle (use Submit)
// or unsupported (caller may queue or interrupt+submit).
func (a *Agent) Steer(text string) (DeliveryOutcome, error)

// Interrupt cancels the open turn (hard stop), then does NOT auto-send — caller
// sends separately unless using SendMode(..., DeliveryInterrupt) which cancel+submits.
func (a *Agent) Interrupt() error

// SendMode(..., DeliveryInterrupt): Interrupt then Submit if text non-empty.
```

**Invariants:**

1. At most **one settling turn** per seat at a time (unchanged).
2. **Steer** never calls `session/cancel` on Cursor when native supersede works.
3. **Interrupt** always uses the provider's hard-stop path.
4. **Queue** is durable on the jevons side for owner chat; broker `mode=queue` is
   optional ack-only for fleet agents.

---

## Broker wire (extends `send`)

```json
{
  "type": "send",
  "send": {
    "name": "jevons-po",
    "text": "…",
    "mode": "steer"
  }
}
```

`mode` enum: `submit` (default) | `steer` | `interrupt` | `queue`.

Response augments `sent` body:

```json
{
  "type": "sent",
  "sent": {
    "name": "jevons-po",
    "mode": "steer",
    "mechanism": "acp_session_prompt_supersede",
    "phase_before": "in_turn"
  }
}
```

`agent_info` / grant snapshot adds `turn_caps` for UI hinting.

Golden wire vectors required (🎯T2.10 family).

---

## Backend mapping

| Provider | Interrupt | Steer (preferred) | Steer fallback |
|----------|-----------|-------------------|----------------|
| **Cursor ACP** | `session/cancel` | Second `session/prompt`; track multiple prompt IDs; superseded RPC → `cancelled`, merged turn continues | Queue until prompt response |
| **Grok ACP** | `session/cancel` | Second `session/prompt` (finish-slice policy) | Queue |
| **Codex app-server** | `turn/interrupt` | `turn/steer` when present on installed CLI | Queue |
| **Claude tmux** | ESC / interrupt op | Composer inject if product exposes it; else **queue_until_idle** (label honestly) | Interrupt+resend (stuck only) |
| **Bedrock / Ollama** | provider op if any | Usually **none** → queue | — |

### ACP steer implementation sketch (Cursor/Grok)

Remove client-side `prompt already in flight` **reject** for steer path only:

- Maintain `promptIDs []int64` stack; top settles turn.
- On steer: push new id, write second `session/prompt`.
- On early RPC result for non-top id: record superseded, **do not** emit terminal
  assistant stop for seat.
- On top id completion: pop, emit terminal, seat → idle.

Submit while in_turn without steer mode: return typed busy (`ErrTurnInFlight`) so
host queues — **do not** auto-steer.

---

## Jevons integration

### Convomux / HTTP / WS

Owner chat POST accepts `mode` parallel to broker. Fleet MCP:

- `jevons_agent_send` gains `mode=steer|interrupt|submit|queue` (default submit).
- `interrupt=true` remains alias for `mode=interrupt` (deprecated in docs).

### Composer (react)

Replace `classifyEnterAction` mapping:

| Chord | Action |
|-------|--------|
| Enter | submit (enqueue if busy) |
| ⌘Enter | steer |
| ⌘⇧Enter | interrupt |
| Shift+Enter | newline |
| Alt+↑/↓ | queue focus cycle, else history recall |

Update oracle families T113/T132/T241/T644 and add queue-focus tests.

### PO layer

Product owners use the same modes when briefing seats:

```text
jevons_agent_send name=jv-… mode=steer text="…"
```

Hermetic: mock claudia caps; assert convomux picks mechanism field in events.

---

## Migration / compatibility

1. Land Claudia API + Cursor/Grok steer first.
2. Jevons composer + convomux modes.
3. Narrow fleet sendq: ACP seats use steer instead of drain-to-uncertain on busy.
4. Deprecate Cmd+Enter interrupt docs; keep `interrupt=true` for one release.

---

## Acceptance (claudia umbrella)

- `Agent.Steer`, `Agent.SendMode`, `TurnCaps` documented in agents-guide + STABILITY.
- Cursor ACP live: mid-turn steer changes agent direction without `session/cancel`.
- Broker golden vectors for send.mode.
- Hermetic: superseded prompt id does not end turn early.

## Acceptance (jevons child)

- Composer chords match table above on development `:13705`.
- Alt+↑/↓ focuses queue before transcript history.
- `jevons_agent_send` mode=steer reaches broker with mechanism in status event.
- Journey/oracle updates green.
