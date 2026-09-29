package ui

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// The panic screen has to survive being looked at. A frozen wall of text
// does not: the giveaway is that nothing moves. So the cover is a small
// engine that plays a plausible agent session — prose streams in a word at
// a time, tool calls land one after another, results arrive after a beat,
// and when a task finishes another one starts. It never repeats the same
// opening twice in a row.

const coverTick_ = 60 * time.Millisecond

type coverKind int

const (
	coverPrompt coverKind = iota // "> what the human asked"
	coverProse                   // the agent's reply, streamed
	coverTool                    // ● Tool(args)
	coverResult                  // ⎿ result line under the last tool
	coverThink                   // spinner-only pause, no output
)

type coverStep struct {
	kind coverKind
	head string
	body string
	// wait is how long to sit before this step starts producing output.
	wait time.Duration
	// verb labels the spinner while this step is pending.
	verb string
}

// coverState is the running animation.
type coverState struct {
	steps   []coverStep
	index   int           // step being played
	shown   []string      // finished lines, already rendered
	partial string        // the part of the current body revealed so far
	waited  time.Duration // time spent waiting before the current step
	done    bool

	paused    bool
	verb      string
	started   time.Time
	tokens    int
	spinFrame int
	rng       *rand.Rand
	lastPick  int
}

func newCoverState() *coverState {
	c := &coverState{
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
		started:  time.Now(),
		lastPick: -1,
	}
	c.enqueueScenario()
	return c
}

func (c *coverState) enqueueScenario() {
	pick := c.rng.Intn(len(coverScenarios))
	if len(coverScenarios) > 1 {
		for pick == c.lastPick {
			pick = c.rng.Intn(len(coverScenarios))
		}
	}
	c.lastPick = pick
	c.steps = append(c.steps, coverScenarios[pick]()...)
}

// advance moves the animation on by one tick and reports whether anything
// changed, so the view is only rebuilt when it has to be.
func (c *coverState) advance() bool {
	if c.paused {
		return false
	}
	c.spinFrame++
	if c.index >= len(c.steps) {
		// the session ran out: start another piece of work rather than
		// leaving a dead screen
		c.enqueueScenario()
	}
	step := c.steps[c.index]
	c.verb = step.verb

	if c.waited < step.wait {
		c.waited += coverTick_
		// tokens keep ticking up while "thinking", as they would
		c.tokens += 7 + c.rng.Intn(23)
		return c.spinFrame%4 == 0 // only redraw for the spinner frame
	}

	switch step.kind {
	case coverThink:
		c.finishStep("")
		return true

	case coverTool:
		c.finishStep(toolGreen.Render("● ") + step.head)
		return true

	case coverResult:
		c.finishStep(dim.Render("  ⎿  ") + dim.Render(step.body))
		return true

	case coverPrompt:
		c.finishStep(dim.Render("> ") + step.body)
		return true

	default: // coverProse — streamed a few characters at a time
		full := step.body
		if len(c.partial) >= len(full) {
			c.finishStep(c.partial)
			return true
		}
		step := 3 + c.rng.Intn(6)
		end := len(c.partial) + step
		if end > len(full) {
			end = len(full)
		}
		// do not split a multi-byte rune
		for end < len(full) && (full[end]&0xC0) == 0x80 {
			end++
		}
		c.partial = full[:end]
		c.tokens += 2 + c.rng.Intn(6)
		return true
	}
}

func (c *coverState) finishStep(line string) {
	if line != "" {
		c.shown = append(c.shown, line)
	}
	c.partial = ""
	c.waited = 0
	c.index++
	// keep the transcript from growing without bound over a long cover
	if len(c.shown) > 400 {
		c.shown = c.shown[len(c.shown)-300:]
	}
}

// pause freezes the session the way esc interrupts the real one, leaving
// the transcript standing and the spinner gone. Resuming reads as the
// obvious next thing: asking it to carry on.
func (c *coverState) pause() {
	if c.paused {
		return
	}
	c.paused = true
	if c.partial != "" {
		c.shown = append(c.shown, c.partial)
		c.partial = ""
	}
	c.shown = append(c.shown, dim.Render("  ⎿  Interrupted by user"))
}

func (c *coverState) resume() {
	if !c.paused {
		return
	}
	c.paused = false
	c.shown = append(c.shown, "", dim.Render("> ")+"continue")
	c.waited = 0
}

func (c *coverState) togglePause() {
	if c.paused {
		c.resume()
		return
	}
	c.pause()
}

// render lays the session out at the given width.
func (c *coverState) render(width int) string {
	wrap := lipgloss.NewStyle().Width(max(30, width))
	var b strings.Builder
	for _, line := range c.shown {
		b.WriteString(wrap.Render(line) + "\n")
		if strings.HasPrefix(line, dim.Render("> ")) {
			b.WriteString("\n")
		}
	}
	if c.partial != "" {
		b.WriteString(wrap.Render(c.partial) + "\n")
	}
	return b.String()
}

// status is the spinner line: elapsed time and a token count that only
// ever climbs, the way the real one does.
func (c *coverState) status() string {
	if c.paused {
		// no spinner while stopped: that is what an interrupted session
		// looks like, and it doubles as the "paused" indicator
		return ""
	}
	frame := spinnerFrames[c.spinFrame/4%len(spinnerFrames)]
	verb := c.verb
	if verb == "" {
		verb = "Working"
	}
	elapsed := int(time.Since(c.started).Seconds())
	return claude.Render(frame+" "+verb+"… ") +
		dim.Render(fmt.Sprintf("(%ds · ↓ %s tokens · esc to interrupt)", elapsed, humanTokens(c.tokens)))
}

// --- scenarios ---
//
// Each is a function so every play gets fresh randomised numbers in the
// tool output; two runs of the same scenario never read identically.

type scenario func() []coverStep

var coverScenarios = []scenario{scenarioBackoff, scenarioFlakyTest, scenarioIndex, scenarioAlloc, scenarioMigration}

func jitter(base time.Duration) time.Duration {
	return base + time.Duration(rand.Intn(400))*time.Millisecond
}

func scenarioBackoff() []coverStep {
	files := 4 + rand.Intn(6)
	matches := 9 + rand.Intn(20)
	return []coverStep{
		{kind: coverPrompt, body: "the webhook handler retries forever when the provider 502s. what's the right backoff?", wait: jitter(300), verb: "Reading"},
		{kind: coverThink, wait: jitter(900), verb: "Thinking"},
		{kind: coverTool, head: "Search" + dim.Render("(pattern: \"retry|backoff\", path: internal/)"), wait: jitter(200), verb: "Searching"},
		{kind: coverResult, body: fmt.Sprintf("%d matches across %d files", matches, files), wait: jitter(600), verb: "Searching"},
		{kind: coverTool, head: "Read" + dim.Render("(internal/webhook/handler.go)"), wait: jitter(300), verb: "Reading"},
		{kind: coverResult, body: fmt.Sprintf("Read %d lines", 140+rand.Intn(180)), wait: jitter(500), verb: "Reading"},
		{kind: coverProse, body: "The loop at handler.go:88 retries on any non-2xx with a fixed 500ms sleep and no ceiling, so a provider outage turns into an unbounded hot loop. Three things to change: exponential backoff with full jitter capped at 30s, a maximum attempt count after which the event goes to the dead-letter table, and retrying only on 429/5xx and network errors — a 4xx will never succeed on replay.", wait: jitter(700), verb: "Thinking"},
		{kind: coverTool, head: "Update" + dim.Render("(internal/webhook/handler.go)"), wait: jitter(800), verb: "Editing"},
		{kind: coverResult, body: "Updated with 34 additions and 11 removals", wait: jitter(700), verb: "Editing"},
		{kind: coverTool, head: "Bash" + dim.Render("(go test ./internal/webhook/...)"), wait: jitter(400), verb: "Testing"},
		{kind: coverResult, body: fmt.Sprintf("ok  	gbike/internal/webhook	%d.%03ds", rand.Intn(3), rand.Intn(999)), wait: jitter(1800), verb: "Testing"},
		{kind: coverProse, body: "Tests pass. The dead-letter path needs its own counter too — a silent drain otherwise looks identical to healthy traffic.", wait: jitter(600), verb: "Thinking"},
		{kind: coverThink, wait: jitter(2500), verb: "Waiting"},
	}
}

func scenarioFlakyTest() []coverStep {
	runs := 20 + rand.Intn(80)
	fails := 1 + rand.Intn(4)
	return []coverStep{
		{kind: coverPrompt, body: "TestConsumerRebalance fails maybe one run in twenty on CI. can you find it?", wait: jitter(300), verb: "Reading"},
		{kind: coverTool, head: "Bash" + dim.Render(fmt.Sprintf("(go test -run TestConsumerRebalance -count=%d ./...)", runs)), wait: jitter(900), verb: "Reproducing"},
		{kind: coverResult, body: fmt.Sprintf("%d/%d runs failed", fails, runs), wait: jitter(2600), verb: "Reproducing"},
		{kind: coverTool, head: "Read" + dim.Render("(internal/consumer/rebalance_test.go)"), wait: jitter(300), verb: "Reading"},
		{kind: coverResult, body: fmt.Sprintf("Read %d lines", 90+rand.Intn(120)), wait: jitter(500), verb: "Reading"},
		{kind: coverProse, body: "It is an ordering assumption, not a timing one. The test asserts partitions come back in the order they were assigned, but the rebalance callback fires from two goroutines and the slice is appended to without a lock. Under -race it fails every time.", wait: jitter(800), verb: "Thinking"},
		{kind: coverTool, head: "Bash" + dim.Render("(go test -race -run TestConsumerRebalance ./internal/consumer/)"), wait: jitter(400), verb: "Testing"},
		{kind: coverResult, body: "DATA RACE detected — rebalance.go:214 write, rebalance.go:231 read", wait: jitter(2200), verb: "Testing"},
		{kind: coverTool, head: "Update" + dim.Render("(internal/consumer/rebalance.go)"), wait: jitter(600), verb: "Editing"},
		{kind: coverResult, body: "Updated with 9 additions and 3 removals", wait: jitter(600), verb: "Editing"},
		{kind: coverProse, body: "Sorting the assignment before asserting, and guarding the append, makes it deterministic. Running it a hundred times to be sure.", wait: jitter(500), verb: "Thinking"},
		{kind: coverTool, head: "Bash" + dim.Render("(go test -race -count=100 -run TestConsumerRebalance ./internal/consumer/)"), wait: jitter(400), verb: "Testing"},
		{kind: coverResult, body: "ok  	gbike/internal/consumer	18.442s", wait: jitter(3000), verb: "Testing"},
		{kind: coverThink, wait: jitter(2500), verb: "Waiting"},
	}
}

func scenarioIndex() []coverStep {
	rows := 2 + rand.Intn(40)
	return []coverStep{
		{kind: coverPrompt, body: "the rides list query got slow this week. anything obvious?", wait: jitter(300), verb: "Reading"},
		{kind: coverTool, head: "Bash" + dim.Render("(psql -c \"explain analyze select ... from rides where ...\")"), wait: jitter(700), verb: "Profiling"},
		{kind: coverResult, body: fmt.Sprintf("Seq Scan on rides  (cost=0.00..%d.%02d rows=%d width=148)", 18000+rand.Intn(9000), rand.Intn(99), rows*1000), wait: jitter(1900), verb: "Profiling"},
		{kind: coverProse, body: "It stopped using the index. The predicate wraps created_at in a timezone conversion, so the btree on created_at no longer matches — Postgres falls back to a sequential scan over every row.", wait: jitter(700), verb: "Thinking"},
		{kind: coverTool, head: "Grep" + dim.Render("(pattern: \"created_at AT TIME ZONE\", path: internal/store/)"), wait: jitter(300), verb: "Searching"},
		{kind: coverResult, body: fmt.Sprintf("%d matches in 2 files", 2+rand.Intn(5)), wait: jitter(500), verb: "Searching"},
		{kind: coverProse, body: "Two options: store the column as timestamptz and compare against a converted bound instead, or add an expression index matching the predicate. The first is cheaper to maintain and fixes the other query too.", wait: jitter(600), verb: "Thinking"},
		{kind: coverTool, head: "Update" + dim.Render("(internal/store/rides.go)"), wait: jitter(700), verb: "Editing"},
		{kind: coverResult, body: "Updated with 12 additions and 12 removals", wait: jitter(600), verb: "Editing"},
		{kind: coverTool, head: "Bash" + dim.Render("(go test ./internal/store/... -run TestRides)"), wait: jitter(400), verb: "Testing"},
		{kind: coverResult, body: "ok  	gbike/internal/store	2.118s", wait: jitter(2000), verb: "Testing"},
		{kind: coverThink, wait: jitter(2500), verb: "Waiting"},
	}
}

func scenarioAlloc() []coverStep {
	pct := 30 + rand.Intn(50)
	return []coverStep{
		{kind: coverPrompt, body: "decode path is allocating way too much under load. where is it going?", wait: jitter(300), verb: "Reading"},
		{kind: coverTool, head: "Bash" + dim.Render("(go test -bench=BenchmarkDecode -benchmem ./internal/codec/)"), wait: jitter(600), verb: "Benchmarking"},
		{kind: coverResult, body: fmt.Sprintf("BenchmarkDecode-10  %d	 %d ns/op	%d B/op	%d allocs/op", 40000+rand.Intn(90000), 9000+rand.Intn(20000), 4000+rand.Intn(9000), 40+rand.Intn(60)), wait: jitter(2800), verb: "Benchmarking"},
		{kind: coverProse, body: "Most of it is one line: the payload is unmarshalled into a map[string]any and then re-marshalled into the struct. That round trip allocates for every key. Decoding straight into the struct with a typed decoder skips it entirely.", wait: jitter(800), verb: "Thinking"},
		{kind: coverTool, head: "Update" + dim.Render("(internal/codec/decode.go)"), wait: jitter(700), verb: "Editing"},
		{kind: coverResult, body: "Updated with 41 additions and 58 removals", wait: jitter(700), verb: "Editing"},
		{kind: coverTool, head: "Bash" + dim.Render("(go test -bench=BenchmarkDecode -benchmem ./internal/codec/)"), wait: jitter(400), verb: "Benchmarking"},
		{kind: coverResult, body: fmt.Sprintf("BenchmarkDecode-10  %d	 %d ns/op	%d B/op	%d allocs/op", 120000+rand.Intn(90000), 2000+rand.Intn(4000), 600+rand.Intn(900), 6+rand.Intn(10)), wait: jitter(2600), verb: "Benchmarking"},
		{kind: coverProse, body: fmt.Sprintf("Allocations down about %d%% and the benchmark is roughly three times faster. Behaviour is unchanged — the existing table tests all still pass.", pct), wait: jitter(600), verb: "Thinking"},
		{kind: coverThink, wait: jitter(2500), verb: "Waiting"},
	}
}

func scenarioMigration() []coverStep {
	return []coverStep{
		{kind: coverPrompt, body: "review the config loader change before I merge it", wait: jitter(300), verb: "Reading"},
		{kind: coverTool, head: "Bash" + dim.Render("(git diff --stat main...HEAD)"), wait: jitter(400), verb: "Reading"},
		{kind: coverResult, body: fmt.Sprintf(" %d files changed, %d insertions(+), %d deletions(-)", 3+rand.Intn(9), 60+rand.Intn(200), 20+rand.Intn(120)), wait: jitter(700), verb: "Reading"},
		{kind: coverTool, head: "Read" + dim.Render("(internal/config/loader.go)"), wait: jitter(300), verb: "Reviewing"},
		{kind: coverResult, body: fmt.Sprintf("Read %d lines", 120+rand.Intn(200)), wait: jitter(600), verb: "Reviewing"},
		{kind: coverProse, body: "The embed.FS move looks right, but there is one behaviour change worth catching: the old loader treated a missing optional file as an empty config, and the new one returns fs.ErrNotExist. Anything relying on the old default will fail to start rather than fall back.", wait: jitter(900), verb: "Reviewing"},
		{kind: coverTool, head: "Grep" + dim.Render("(pattern: \"LoadOptional|config.Load\\\\(\", path: cmd/)"), wait: jitter(300), verb: "Searching"},
		{kind: coverResult, body: fmt.Sprintf("%d matches in %d files", 3+rand.Intn(6), 2+rand.Intn(4)), wait: jitter(600), verb: "Searching"},
		{kind: coverProse, body: "Two call sites depend on it. Either keep the fallback inside the loader or update both — the loader is the safer place, since a third caller will make the same assumption.", wait: jitter(700), verb: "Thinking"},
		{kind: coverThink, wait: jitter(2500), verb: "Waiting"},
	}
}

// --- interaction ---
//
// The cover is convincing right up until someone watches you type into it
// and nothing happens. So it answers: whatever is entered is echoed as a
// prompt and a plausible reply streams back. Nothing typed here can reach
// a real conversation — the cover engine is the only thing listening.

func (c *coverState) ask(prompt string) {
	steps := []coverStep{
		{kind: coverPrompt, body: prompt, wait: 0, verb: "Thinking"},
		{kind: coverThink, wait: jitter(700), verb: pickVerb()},
	}
	steps = append(steps, replyFor(prompt)...)
	// play the answer next, ahead of whatever was queued
	rest := append([]coverStep{}, c.steps[c.index:]...)
	c.steps = append(c.steps[:c.index], append(steps, rest...)...)
	c.partial = ""
	c.waited = 0
}

func pickVerb() string {
	verbs := []string{"Thinking", "Puzzling", "Considering", "Digging", "Untangling", "Working"}
	return verbs[rand.Intn(len(verbs))]
}

// replyFor answers in a way that fits the shape of the question without
// pretending to understand it: a question gets prose, anything that smells
// like an instruction gets a tool call and a result.
func replyFor(prompt string) []coverStep {
	lower := strings.ToLower(prompt)
	asksQuestion := strings.Contains(prompt, "?") ||
		strings.HasPrefix(lower, "why") || strings.HasPrefix(lower, "what") ||
		strings.HasPrefix(lower, "how") || strings.HasPrefix(lower, "where") ||
		strings.HasPrefix(lower, "should")

	if asksQuestion {
		return []coverStep{
			{kind: coverTool, head: "Search" + dim.Render(fmt.Sprintf("(pattern: %q, path: internal/)", searchTerm(prompt))), wait: jitter(300), verb: "Searching"},
			{kind: coverResult, body: fmt.Sprintf("%d matches across %d files", 3+rand.Intn(22), 2+rand.Intn(7)), wait: jitter(700), verb: "Searching"},
			{kind: coverProse, body: answers[rand.Intn(len(answers))], wait: jitter(600), verb: "Thinking"},
			{kind: coverThink, wait: jitter(2200), verb: "Waiting"},
		}
	}
	return []coverStep{
		{kind: coverTool, head: "Read" + dim.Render(fmt.Sprintf("(internal/%s.go)", pathWord())), wait: jitter(300), verb: "Reading"},
		{kind: coverResult, body: fmt.Sprintf("Read %d lines", 80+rand.Intn(260)), wait: jitter(600), verb: "Reading"},
		{kind: coverProse, body: actions[rand.Intn(len(actions))], wait: jitter(700), verb: "Thinking"},
		{kind: coverTool, head: "Update" + dim.Render(fmt.Sprintf("(internal/%s.go)", pathWord())), wait: jitter(800), verb: "Editing"},
		{kind: coverResult, body: fmt.Sprintf("Updated with %d additions and %d removals", 4+rand.Intn(40), rand.Intn(20)), wait: jitter(700), verb: "Editing"},
		{kind: coverTool, head: "Bash" + dim.Render("(go test ./... )"), wait: jitter(400), verb: "Testing"},
		{kind: coverResult, body: fmt.Sprintf("ok  	gbike/internal/%s	%d.%03ds", pathWord(), rand.Intn(4), rand.Intn(999)), wait: jitter(2200), verb: "Testing"},
		{kind: coverThink, wait: jitter(2200), verb: "Waiting"},
	}
}

// searchTerm lifts the longest word out of the prompt so the fake search
// looks like it was derived from what was asked.
func searchTerm(prompt string) string {
	best := ""
	for _, word := range strings.Fields(prompt) {
		word = strings.Trim(word, "?.,:;\"'()")
		if len(word) > len(best) && len(word) < 24 {
			best = word
		}
	}
	if best == "" {
		return "handler"
	}
	return strings.ToLower(best)
}

func pathWord() string {
	words := []string{"store", "consumer", "webhook", "codec", "config", "ride", "billing", "auth"}
	return words[rand.Intn(len(words))]
}

var answers = []string{
	"Short version: it is the retry path, not the handler. The context is cancelled as soon as the first attempt returns, so every retry after that fails instantly with context.Canceled and the error you see is the last one, not the real one.",
	"Because the deadline is set on the parent context rather than per attempt. Each retry inherits what is left of the original budget, so by the third attempt there is no time to spare and it fails before the request leaves the process.",
	"They are independent — the queue drains in order but the workers do not, so anything that assumes ordering downstream will see gaps. If order matters it has to be enforced at the consumer, not assumed from the producer.",
	"It only shows up under load because the pool is sized to the number of cores. With fewer than eight the contention never surfaces, which is why nobody hit it locally.",
	"I would keep it in the loader. Two call sites depend on the fallback today and a third will make the same assumption tomorrow; pushing the default down means nobody has to remember it.",
	"The index is there but the predicate does not match it — wrapping the column in a conversion makes it unusable, so it falls back to a sequential scan over the whole table.",
}

var actions = []string{
	"The change is small but it moves where the error is handled: the caller should not have to know that a partial write is retryable. Wrapping it at the boundary keeps that detail in one place.",
	"This needs a guard rather than a rewrite — the slice is appended to from two goroutines, and everything else about the function is fine.",
	"Pulling the deadline into each attempt is enough. The rest of the retry logic already backs off properly, it was just sharing one budget across every try.",
	"Replacing the map round trip with a typed decoder removes most of the allocations without changing behaviour, and the table tests cover it already.",
}
