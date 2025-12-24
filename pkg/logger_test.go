package pkg

import (
	"context"
	"sync"
	"testing"
	"time"

	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// fakeCounter registra le chiamate a Add per ispezione nei test
type counterCall struct {
	ctx   context.Context
	value int64
}

type fakeCounter struct {
	name  string
	calls *[]counterCall
}

func (f *fakeCounter) Add(ctx context.Context, value int64, _ ...metric.AddOption) {
	*f.calls = append(*f.calls, counterCall{ctx: ctx, value: value})
}

// implementa le interfacce usate (Int64CounterLike e Int64UpDownCounterLike hanno lo stesso metodo Add)

// fakeMeter crea strumenti fake che registrano le chiamate
type fakeMeter struct {
	counters map[string]*[]counterCall
}

func newFakeMeter() *fakeMeter {
	return &fakeMeter{counters: map[string]*[]counterCall{}}
}

func (m *fakeMeter) Int64Counter(name string, _ ...metric.InstrumentOption) (Int64CounterLike, error) {
	arr := make([]counterCall, 0)
	m.counters[name] = &arr
	return &fakeCounter{name: name, calls: m.counters[name]}, nil
}

func (m *fakeMeter) Int64UpDownCounter(name string, _ ...metric.InstrumentOption) (Int64UpDownCounterLike, error) {
	arr := make([]counterCall, 0)
	m.counters[name] = &arr
	// riuso fakeCounter poiché Add ha la stessa forma
	return &fakeCounter{name: name, calls: m.counters[name]}, nil
}

// fakeSlogHandler cattura i record inoltrati e permette di sincronizzare i test
type fakeSlogHandler struct {
	mu      sync.Mutex
	records []slog.Record
	wg      *sync.WaitGroup
}

func newFakeSlogHandler() *fakeSlogHandler {
	return &fakeSlogHandler{}
}

func (f *fakeSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
	if f.wg != nil {
		f.wg.Done()
	}
	return nil
}

func (f *fakeSlogHandler) Enabled(ctx context.Context, level slog.Level) bool { return true }
func (f *fakeSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler           { return f }
func (f *fakeSlogHandler) WithGroup(name string) slog.Handler                 { return f }

// helper: aspetta fino a timeout che il fake handler abbia almeno n record
func waitForRecords(t *testing.T, fh *fakeSlogHandler, n int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		fh.mu.Lock()
		cnt := len(fh.records)
		fh.mu.Unlock()
		if cnt >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d records, got %d", n, len(fh.records))
}

// Test 1: forwarding when no trace present
func TestForwardingNoTrace(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "no-trace", 0)
	var ctx = context.Background()
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg
	_ = h.Handle(ctx, r)
	// aspetta che next riceva
	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()
	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for next.Handle for no-trace record")
	}
}

// Test 2: buffering + EndSpanSuccess (should cleanup without flush)
func TestBufferingEndSpanSuccess(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	// costruisco un ctx con SpanContext valido contenente un traceID
	var tid trace.TraceID
	copy(tid[:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	var sid trace.SpanID
	copy(sid[:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	// sanity check sulla validità del contesto
	if !sc.IsValid() {
		t.Fatalf("constructed span context is invalid")
	}

	// Detect unexpected forwards: se next.Handle viene chiamato quando non dovrebbe,
	// il WaitGroup si decrementarà e il test fallirà.
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "buffered", 0)
	_ = h.Handle(ctx, r)

	// aspetta che il buffer sia stato creato dal worker
	bufferCreated := false
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		_, bufferCreated = h.buffers[tid.String()]
		h.mu.Unlock()
		if bufferCreated {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !bufferCreated {
		t.Fatalf("buffer for trace %s was not created in time", tid.String())
	}

	// aspetta brevemente per verificare che NON sia stato forwarded
	select {
	case <-func() chan struct{} {
		c := make(chan struct{})
		go func() {
			wg.Wait()
			close(c)
		}()
		return c
	}():
		// raccolgo dettagli sul record inoltrato
		next.mu.Lock()
		var msg string
		if len(next.records) > 0 {
			msg = next.records[0].Message
		}
		next.mu.Unlock()
		t.Fatalf("unexpected forward to next.Handle immediately after Handle; forwarded message=%q", msg)
	case <-time.After(100 * time.Millisecond):
		// ok, non è stato forwarded
	}

	// chiudo lo span con successo: non deve flushare i record
	h.EndSpanSuccess(ctx, tid.String())

	// attendiamo ancora un breve periodo per assicurarci che non arrivi nulla
	wg2 := &sync.WaitGroup{}
	wg2.Add(1)
	next.wg = wg2
	select {
	case <-func() chan struct{} {
		c := make(chan struct{})
		go func() {
			wg2.Wait()
			close(c)
		}()
		return c
	}():
		// raccolgo dettagli sul record inoltrato
		next.mu.Lock()
		var msg2 string
		if len(next.records) > 0 {
			msg2 = next.records[0].Message
		}
		next.mu.Unlock()
		t.Fatalf("unexpected forward to next.Handle after EndSpanSuccess; forwarded message=%q", msg2)
	case <-time.After(100 * time.Millisecond):
		// ok, non è stato forwarded
	}
}

// Test 3: immediate flush on error level
func TestImmediateFlushOnError(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	var tid trace.TraceID
	copy(tid[:], []byte{2, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	var sid trace.SpanID
	copy(sid[:], []byte{2, 2, 3, 4, 5, 6, 7, 8})
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	r := slog.NewRecord(time.Now(), slog.LevelError, "errnow", 0)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg
	_ = h.Handle(ctx, r)
	// Wait for flush
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for immediate flush on error level")
	}
}

// Test 4: timeout triggers flush
func TestTimeoutTriggersFlush(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 50*time.Millisecond)
	defer h.Close()

	var tid trace.TraceID
	copy(tid[:], []byte{3, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	var sid trace.SpanID
	copy(sid[:], []byte{3, 2, 3, 4, 5, 6, 7, 8})
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "to-be-timed-out", 0)
	_ = h.Handle(ctx, r)
	// aspetta che il timer faccia il flush
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for timeout-based flush")
	}
}

// Test 5: drop when channel is full
func TestDropWhenChannelFull(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	// create handler with small buffer to provoke drops without reassigning channel
	h := NewAsyncBufferedHandler(next, fm, 1, 200*time.Millisecond)
	defer h.Close()

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "droptest", 0)
	// rapidly call Handle many times to trigger drops
	for i := 0; i < 200; i++ {
		_ = h.Handle(context.Background(), r)
	}
	// slight wait to allow async counters to be updated
	time.Sleep(50 * time.Millisecond)

	// verifico che il contatore di drop sul fake meter sia stato incrementato
	calls := fm.counters["logging_dropped_operations"]
	if calls == nil || len(*calls) == 0 {
		t.Fatalf("expected dropCounter.Add to be called at least once, got 0 calls")
	}
}

// --- TESTS PER CASI METER == nil ---

// Test A: forwarding when no trace present and meter is nil
func TestForwardingNoTrace_MeterNil(t *testing.T) {
	var fm MyMeter = nil
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "no-trace-nilmeter", 0)
	var ctx = context.Background()
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg
	_ = h.Handle(ctx, r)
	// aspetta che next riceva
	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()
	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for next.Handle for no-trace record with nil meter")
	}
}

// Test B: buffering + EndSpanSuccess with meter nil (should cleanup without flush)
func TestBufferingEndSpanSuccess_MeterNil(t *testing.T) {
	var fm MyMeter = nil
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	// costruisco un ctx con SpanContext valido contenente un traceID
	var tid trace.TraceID
	copy(tid[:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	var sid trace.SpanID
	copy(sid[:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	// sanity check
	if !sc.IsValid() {
		t.Fatalf("constructed span context is invalid")
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "buffered-nilmeter", 0)
	_ = h.Handle(ctx, r)

	// aspetta che il buffer sia stato creato dal worker
	bufferCreated := false
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		_, bufferCreated = h.buffers[tid.String()]
		h.mu.Unlock()
		if bufferCreated {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !bufferCreated {
		t.Fatalf("buffer for trace %s was not created in time (nil meter)", tid.String())
	}

	// aspetta brevemente per verificare che NON sia stato forwarded
	select {
	case <-func() chan struct{} {
		c := make(chan struct{})
		go func() {
			wg.Wait()
			close(c)
		}()
		return c
	}():
		// raccolgo dettagli sul record inoltrato
		next.mu.Lock()
		var msg string
		if len(next.records) > 0 {
			msg = next.records[0].Message
		}
		next.mu.Unlock()
		t.Fatalf("unexpected forward to next.Handle immediately after Handle; forwarded message=%q", msg)
	case <-time.After(100 * time.Millisecond):
		// ok, non è stato forwarded
	}

	// chiudo lo span con successo: non deve flushare i record
	h.EndSpanSuccess(ctx, tid.String())

	// attendiamo ancora un breve periodo per assicurarci che non arrivi nulla
	wg2 := &sync.WaitGroup{}
	wg2.Add(1)
	next.wg = wg2
	select {
	case <-func() chan struct{} {
		c := make(chan struct{})
		go func() {
			wg2.Wait()
			close(c)
		}()
		return c
	}():
		// raccolgo dettagli sul record inoltrato
		next.mu.Lock()
		var msg2 string
		if len(next.records) > 0 {
			msg2 = next.records[0].Message
		}
		next.mu.Unlock()
		t.Fatalf("unexpected forward to next.Handle after EndSpanSuccess (nil meter); forwarded message=%q", msg2)
	case <-time.After(100 * time.Millisecond):
		// ok, non è stato forwarded
	}
}

// Test C: immediate flush on error level with meter nil
func TestImmediateFlushOnError_MeterNil(t *testing.T) {
	var fm MyMeter = nil
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	var tid trace.TraceID
	copy(tid[:], []byte{2, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	var sid trace.SpanID
	copy(sid[:], []byte{2, 2, 3, 4, 5, 6, 7, 8})
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	r := slog.NewRecord(time.Now(), slog.LevelError, "errnow-nilmeter", 0)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg
	_ = h.Handle(ctx, r)
	// Wait for flush
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for immediate flush on error level with nil meter")
	}
}

// Test D: drop when channel is full with meter nil (should not panic)
func TestDropWhenChannelFull_MeterNil(t *testing.T) {
	var fm MyMeter = nil
	next := newFakeSlogHandler()
	// create handler with small buffer to provoke drops without reassigning channel
	h := NewAsyncBufferedHandler(next, fm, 1, 200*time.Millisecond)
	defer h.Close()

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "droptest-nilmeter", 0)
	// rapidly call Handle many times to trigger drops
	for i := 0; i < 200; i++ {
		_ = h.Handle(context.Background(), r)
	}
	// slight wait to allow async processing
	time.Sleep(50 * time.Millisecond)
	// if we reach here without panic the behavior is acceptable; assert no records nil
	// ensure next handler didn't receive a negative number of records (sanity)
	next.mu.Lock()
	cnt := len(next.records)
	next.mu.Unlock()
	if cnt < 0 {
		t.Fatalf("invalid records count: %d", cnt)
	}
}

// Test: per-span custom timeout triggers flush at custom duration
func TestPerSpanCustomTimeout(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	// default lungo, user custom corto
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	ctx := context.Background()
	custom := 50 * time.Millisecond
	traceID, err := h.StartSpan(&ctx, &custom, nil, nil)
	if err != nil {
		t.Fatalf("StartSpan error: %v", err)
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "per-span-custom", 0)
	_ = h.Handle(ctx, r)

	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()

	select {
	case <-c:
		// ok: flush avvenuto entro custom timeout
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for custom per-span timeout flush")
	}

	// cleanup
	h.EndSpanSuccess(ctx, traceID)
}

// Test: per-span nil disables timeout (no flush)
func TestPerSpanDisableTimeout(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	// default breve ma lo disabilitiamo per lo span
	h := NewAsyncBufferedHandler(next, fm, 10, 50*time.Millisecond)
	defer h.Close()

	ctx := context.Background()
	// disabilito il timeout per questo span
	traceID, err := h.StartSpan(&ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("StartSpan error: %v", err)
	}

	// ASSERT: StartSpan should have set an entry in perSpanTimeouts (value == nil)
	h.mu.Lock()
	_, ok := h.perSpanTimeouts[traceID]
	h.mu.Unlock()
	if !ok {
		t.Fatalf("StartSpan did not set perSpanTimeouts for trace %s", traceID)
	}

	// preparo next per rilevare eventuale forward (non ci deve essere)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "per-span-disabled", 0)
	_ = h.Handle(ctx, r)

	// aspettiamo un tempo maggiore del default: se riceviamo qualcosa è un errore
	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()

	select {
	case <-c:
		t.Fatal("unexpected flush: per-span timeout was disabled but flush occurred")
	case <-time.After(200 * time.Millisecond):
		// ok: nessun flush automatico
	}

	// cleanup esplicito
	h.EndSpanSuccess(ctx, traceID)
}

// Test: per-span pointer to 0 uses handler default timeout
func TestPerSpanZeroUsesDefault(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	// default medio
	defaultTimeout := 60 * time.Millisecond
	h := NewAsyncBufferedHandler(next, fm, 10, defaultTimeout)
	defer h.Close()

	ctx := context.Background()
	zero := time.Duration(0)
	traceID, err := h.StartSpan(&ctx, &zero, nil, nil) // dovrebbe usare defaultTimeout
	if err != nil {
		t.Fatalf("StartSpan error: %v", err)
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "per-span-zero", 0)
	_ = h.Handle(ctx, r)

	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()

	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for default timeout flush when per-span value == 0")
	}

	// cleanup
	h.EndSpanSuccess(ctx, traceID)
}

// Test: per-span minimum log level filtering
func TestPerSpanMinLevelFiltering(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	// costruisco un ctx con SpanContext valido contenente un traceID
	var tid trace.TraceID
	copy(tid[:], []byte{4, 4, 4, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	var sid trace.SpanID
	copy(sid[:], []byte{4, 4, 3, 4, 5, 6, 7, 8})
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	// Imposto il livello minimo a WARN per questo span
	minLevel := slog.LevelWarn
	h.SetSpanMinLevel(tid.String(), &minLevel)

	// 1) Invio un record INFO: non dovrebbe essere inoltrato
	rInfo := slog.NewRecord(time.Now(), slog.LevelInfo, "info-should-be-dropped", 0)
	_ = h.Handle(ctx, rInfo)
	// attendiamo un po' per dare tempo al worker di processare
	time.Sleep(100 * time.Millisecond)
	next.mu.Lock()
	if len(next.records) != 0 {
		next.mu.Unlock()
		t.Fatalf("expected 0 forwarded records for INFO below min level, got %d", len(next.records))
	}
	next.mu.Unlock()

	// 2) Invio un record WARN: dovrebbe essere inoltrato
	rWarn := slog.NewRecord(time.Now(), slog.LevelWarn, "warn-should-pass", 0)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg
	_ = h.Handle(ctx, rWarn)
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for WARN to be forwarded")
	}
	// verifico messaggio
	next.mu.Lock()
	if len(next.records) == 0 {
		next.mu.Unlock()
		t.Fatal("expected at least one forwarded record for WARN")
	}
	if next.records[len(next.records)-1].Message != "warn-should-pass" {
		msg := next.records[len(next.records)-1].Message
		next.mu.Unlock()
		t.Fatalf("unexpected forwarded message per WARN: %q", msg)
	}
	next.mu.Unlock()

	// 3) Rimuovo il filtro e invio INFO: ora dovrebbe essere inoltrato
	h.SetSpanMinLevel(tid.String(), nil)
	rInfo2 := slog.NewRecord(time.Now(), slog.LevelInfo, "info-should-pass-after-remove", 0)
	wg2 := &sync.WaitGroup{}
	wg2.Add(1)
	next.wg = wg2
	_ = h.Handle(ctx, rInfo2)
	c2 := make(chan struct{})
	go func() { wg2.Wait(); close(c2) }()
	select {
	case <-c2:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for INFO to be forwarded after removing min-level filter")
	}
	// verifico messaggio
	next.mu.Lock()
	if len(next.records) == 0 {
		next.mu.Unlock()
		t.Fatal("expected at least one forwarded record after removing filter")
	}
	if next.records[len(next.records)-1].Message != "info-should-pass-after-remove" {
		msg := next.records[len(next.records)-1].Message
		next.mu.Unlock()
		t.Fatalf("unexpected forwarded message after removing filter: %q", msg)
	}
	next.mu.Unlock()
}

// New test: StartSpan can accept minLevel and apply filtering equivalent to SetSpanMinLevel
func TestStartSpanWithMinLevelFiltering(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	ctx := context.Background()

	// Imposto il livello minimo a WARN tramite StartSpan
	minLevel := slog.LevelWarn
	zero := time.Duration(0)
	traceID, err := h.StartSpan(&ctx, &zero, &minLevel, nil)
	if err != nil {
		t.Fatalf("StartSpan error: %v", err)
	}

	// ASSERT: StartSpan should have set perSpanMinLevels for this traceID
	h.mu.Lock()
	if ml, ok := h.perSpanMinLevels[traceID]; !ok || ml == nil {
		h.mu.Unlock()
		t.Fatalf("StartSpan did not set perSpanMinLevels for trace %s", traceID)
	}
	h.mu.Unlock()

	// 1) INFO deve essere scartato
	rInfo := slog.NewRecord(time.Now(), slog.LevelInfo, "info-dropped-by-startspan", 0)
	_ = h.Handle(ctx, rInfo)
	// attendiamo un po' per dare tempo al worker di processare
	time.Sleep(100 * time.Millisecond)
	next.mu.Lock()
	if len(next.records) != 0 {
		next.mu.Unlock()
		t.Fatalf("expected 0 forwarded records for INFO below min level (StartSpan), got %d", len(next.records))
	}
	next.mu.Unlock()

	// 2) WARN deve passare
	rWarn := slog.NewRecord(time.Now(), slog.LevelWarn, "warn-passes-by-startspan", 0)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg
	_ = h.Handle(ctx, rWarn)
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for WARN to be forwarded (StartSpan)")
	}
	// verifico messaggio
	next.mu.Lock()
	if len(next.records) == 0 {
		next.mu.Unlock()
		t.Fatal("expected at least one forwarded record for WARN (StartSpan)")
	}
	if next.records[len(next.records)-1].Message != "warn-passes-by-startspan" {
		msg := next.records[len(next.records)-1].Message
		next.mu.Unlock()
		t.Fatalf("unexpected forwarded message (StartSpan WARN): %q", msg)
	}
	next.mu.Unlock()

	// cleanup
	h.EndSpanSuccess(ctx, traceID)
}

// --- TESTS PER I WRAPPER Debug/Info/Warning/Error ---

// helper: costruisce un traceID valido (16 bytes) e restituisce il suo string
func makeTestTraceID() string {
	var tid trace.TraceID
	copy(tid[:], []byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 11, 12, 13, 14, 15, 16, 17})
	return tid.String()
}

func TestWrappers_DebugInfoWarningError_Forwarding(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	traceID := makeTestTraceID()

	// settiamo wg per aspettare 4 record inoltrati
	wg := &sync.WaitGroup{}
	wg.Add(4)
	next.wg = wg

	// Chiamo i wrapper
	h.Debug(traceID, "debug-msg")
	h.Info(traceID, "info-msg")
	h.Warning(traceID, "warn-msg")
	h.Error(traceID, "error-msg")

	// aspettiamo che i record siano stati inoltrati (i record con traceID valido vanno bufferizzati
	// e poi flushati solo su error/timeout; per semplicità verifichiamo che siano almeno ricevuti dal fake handler)
	waitForRecords(t, next, 4, 500*time.Millisecond)

	// verifichiamo i messaggi ricevuti
	next.mu.Lock()
	defer next.mu.Unlock()
	if len(next.records) < 4 {
		t.Fatalf("expected at least 4 records, got %d", len(next.records))
	}

	// costruiamo una mappa dei messaggi
	msgs := map[string]bool{}
	for _, r := range next.records {
		msgs[r.Message] = true
	}

	expected := []string{"debug-msg", "info-msg", "warn-msg", "error-msg"}
	for _, e := range expected {
		if !msgs[e] {
			t.Fatalf("expected message %q to be forwarded, not found", e)
		}
	}
}

// Test che il wrapper senza traceID forwardi immediatamente al next handler
func TestWrappers_NoTrace_ImmediateForward(t *testing.T) {
	fm := newFakeMeter()
	next := newFakeSlogHandler()
	h := NewAsyncBufferedHandler(next, fm, 10, 200*time.Millisecond)
	defer h.Close()

	wg := &sync.WaitGroup{}
	wg.Add(1)
	next.wg = wg

	// call wrapper with empty traceID -> should immediately forward
	h.Info("", "no-trace-msg")

	c := make(chan struct{})
	go func() {
		wg.Wait()
		close(c)
	}()

	select {
	case <-c:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for immediate forward for no-trace wrapper")
	}
}
