package pkg

import (
	"context"
	"crypto/rand"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ---- INTERFACCE -----

// SpanLogger è un'interfaccia per la gestione dei log di span.
type SpanLogger interface {
	EndSpanSuccess(ctx context.Context, traceID string)
	EndSpanError(ctx context.Context, traceID string, err error)
}

// Int64CounterLike è l'interfaccia minima usata nel package per i contatori
// (compatibile con metric.Int64Counter's Add method)
type Int64CounterLike interface {
	Add(ctx context.Context, value int64, opts ...metric.AddOption)
}

// Int64UpDownCounterLike è l'interfaccia minima usata per gli up/down counter
type Int64UpDownCounterLike interface {
	Add(ctx context.Context, value int64, opts ...metric.AddOption)
}

// MyMeter è un'interfaccia minimale che rappresenta un meter fake per i test
// (fornisce solo gli strumenti che il handler utilizza).
type MyMeter interface {
	Int64Counter(name string, opts ...metric.InstrumentOption) (Int64CounterLike, error)
	Int64UpDownCounter(name string, opts ...metric.InstrumentOption) (Int64UpDownCounterLike, error)
}

// ---- LOGICHE DI COMANDO -----

type OpType int

const (
	OpLog OpType = iota
	OpReleaseSuccess
	OpReleaseError
	OpTimeout
)

type LogCommand struct {
	Op      OpType
	Record  slog.Record
	Context context.Context
	TraceID string
	Tags    []slog.Attr
	Err     error // campo opzionale per EndSpanError
}

// ---- GESTORE ASINCRONO BUFFERIZZATO -----

// AsyncBufferedHandler è un handler che bufferizza i log per traceID e li
// flusha in determinate condizioni (timeout, error, esplicito EndSpanError).
//
// Nuovo: supporta policy di timeout per singolo span tramite la mappa
// perSpanTimeouts: map[traceID]*time.Duration
// - valore == nil    => timeout DISABILITATO per quello span (mai OpTimeout)
// - valore != nil && *v == 0 => usa timeout di default dell'handler
// - valore != nil && *v > 0 => usa durata specificata
//
// Questo permette di avere timeout personalizzati per span oppure disabilitarli
// quando, ad esempio, uno span dura molto a lungo e non si vuole che venga
// flushato automaticamente.
type AsyncBufferedHandler struct {
	next    slog.Handler             // Handler successivo nella catena
	channel chan LogCommand          // Canale per i comandi di log
	mu      *sync.Mutex              // Mutex per la sincronizzazione (puntatore per evitare copie accidentali)
	buffers map[string][]slog.Record // Buffer per i log per traceID
	timers  map[string]*time.Timer   // Timer attivi per traceID
	// perSpanTimeouts mantiene la policy di timeout per ogni traceID:
	// - nil pointer (value == nil) => timeout DISABILITATO per quello span (mai OpTimeout)
	// - non-nil pointer con valore == 0 => usare il timeout di default h.timeoutDuration
	// - non-nil pointer con valore > 0 => usare la durata specificata
	perSpanTimeouts map[string]*time.Duration
	// perSpanMinLevels mantiene il livello minimo di log che verrà bufferizzato/inoltrato
	// per ogni traceID. Valore == nil => nessun filtro (accetta tutti i livelli).
	// Se presente e non-nil, i record con livello < *minLevel verranno scartati.
	perSpanMinLevels map[string]*slog.Level
	// perSpanTags mantiene una lista di attributi (slog.Attr) derivati dai tag
	// (stringhe) passate allo StartSpan per ogni traceID. Viene aggiunta ai record
	// bufferizzati e inoltrati.
	perSpanTags map[string][]slog.Attr
	// timeoutDuration è il valore di default per gli span per i quali non sia
	// stata impostata una policy esplicita (o per i quali la policy punta a 0).
	timeoutDuration time.Duration
	attrs           []slog.Attr // attributi da aggiungere ai record
	meter           MyMeter     // Adattatore del Meter OTel

	// Metriche OTel per Report Temporali
	totalCounter   Int64CounterLike // Contatore totale delle operazioni
	successCounter Int64CounterLike // Contatore delle operazioni di successo
	errorCounter   Int64CounterLike // Contatore delle operazioni di errore
	dropCounter    Int64CounterLike // Contatore delle operazioni scartate per via del buffer pieno

	// Indicatori istantanei
	activeSpansGauge   Int64UpDownCounterLike // Contatore degli span attivi
	activeRecordsGauge Int64UpDownCounterLike // Contatore dei record attivi

	// sincronizzazione per shutdown controllato
	wg        *sync.WaitGroup
	closeOnce *sync.Once
	closed    *int32 // flag atomico condiviso
}

func NewAsyncBufferedHandler(next slog.Handler, meter MyMeter, bufferSize int, timeout time.Duration) *AsyncBufferedHandler {
	h := &AsyncBufferedHandler{
		next:             next,
		channel:          make(chan LogCommand, bufferSize),
		buffers:          make(map[string][]slog.Record),
		timers:           make(map[string]*time.Timer),
		perSpanTimeouts:  make(map[string]*time.Duration), // inizializziamo la mappa
		perSpanMinLevels: make(map[string]*slog.Level),    // inizializziamo la mappa dei min-level per span
		perSpanTags:      make(map[string][]slog.Attr),    // inizializziamo la mappa dei tag per span
		timeoutDuration:  timeout,
		mu:               &sync.Mutex{},
		wg:               &sync.WaitGroup{},
		closeOnce:        &sync.Once{},
		meter:            meter,
	}
	var c int32 = 0
	h.closed = &c

	// Inizializzazione delle metriche usando il meter fornito (manteniamo initMetrics)
	h.initMetrics()

	// avvio worker con waitgroup per permettere un shutdown controllato
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for cmd := range h.channel {
			h.process(cmd)
		}
	}()
	return h
}

// SetMeter permette di impostare o cambiare il meter usato per le metriche
func (h *AsyncBufferedHandler) SetMeter(meter MyMeter) {
	h.meter = meter
	// re-inizializzo le metriche con il nuovo meter
	h.initMetrics()
}

// initMetrics resta intatto: inizializza gli strumenti dal meter se presente.
func (h *AsyncBufferedHandler) initMetrics() {
	if h.meter != nil {
		// Le API del meter ritornano (instrument, error) — qui scartiamo l'errore con _.
		h.totalCounter, _ = h.meter.Int64Counter("logging_total_operations", metric.WithDescription("Somma totale delle operazioni di logging"))
		h.successCounter, _ = h.meter.Int64Counter("logging_success_operations", metric.WithDescription("Contatore delle operazioni di successo"))
		h.errorCounter, _ = h.meter.Int64Counter("logging_error_operations", metric.WithDescription("Contatore delle operazioni di errore"))
		h.dropCounter, _ = h.meter.Int64Counter("logging_dropped_operations", metric.WithDescription("Contatore delle operazioni scartate per via del buffer pieno"))

		h.activeSpansGauge, _ = h.meter.Int64UpDownCounter("logging_active_spans", metric.WithDescription("Contatore degli span attivi"))
		h.activeRecordsGauge, _ = h.meter.Int64UpDownCounter("logging_active_records", metric.WithDescription("Contatore dei record attivi"))
	} else {
		h.totalCounter = nil
		h.successCounter = nil
		h.errorCounter = nil
		h.dropCounter = nil
		h.activeSpansGauge = nil
	}
}

// StartSpan permette di impostare una policy di timeout per uno specifico traceID.
// Il parametro timeout è un puntatore *time.Duration per permettere tre comportamenti:
//   - timeout == nil => timeout DISABILITATO per quello span (mai OpTimeout)
//   - timeout != nil && *timeout == 0 => usare il timeout di default dell'handler
//   - timeout != nil && *timeout > 0 => usare la durata specificata
//
// Nuovo: StartSpan ora genera internamente un traceID univoco (non preso come input),
// accetta anche un parametro opzionale minLevel *slog.Level che, se non-nil, imposta
// il livello minimo di log che verrà bufferizzato/inoltrato per quello span
// (equivalente a chiamare SetSpanMinLevel(traceID, minLevel)). Se minLevel == nil
// non viene modificata la policy di livello esistente per quello span.
//
// Nuovo: StartSpan accetta anche tags []string (nil o vuota) che verranno memorizzate
// per il singolo span e aggiunte come attributi ai record bufferizzati/inoltrati.
//
// Restituisce il traceID generato e un context con lo SpanContext associato che il
// chiamante può usare per inviare log che verranno associati a questo span.
func (h *AsyncBufferedHandler) StartSpan(ctx *context.Context, timeout *time.Duration, minLevel *slog.Level, tags []string) (string, error) {
	// Genero un traceID unico non presente nelle mappe del logger
	var tid trace.TraceID
	var tidStr string
	for {
		// genero 16 byte casuali
		b := make([]byte, 16)
		_, err := rand.Read(b)
		if err != nil {
			// fallback deterministico in caso di errore raro
			now := time.Now().UnixNano()
			copy(b[:], fmt.Sprintf("%016x", now))
		}
		copy(tid[:], b)
		tidStr = tid.String()

		// tieni il lock finché non hai impostato sia il buffer sia le policy per-span
		h.mu.Lock()
		_, inBuffers := h.buffers[tidStr]
		_, inTimers := h.timers[tidStr]
		_, inTimeouts := h.perSpanTimeouts[tidStr]
		_, inMin := h.perSpanMinLevels[tidStr]
		_, inTags := h.perSpanTags[tidStr]
		collision := inBuffers || inTimers || inTimeouts || inMin || inTags
		if !collision {
			// segno il buffer (vuoto) per occupare l'ID e evitare race con altre StartSpan
			h.buffers[tidStr] = make([]slog.Record, 0)
			// use provided context value for metrics; if ctx pointer is nil use background
			var ctxVal context.Context
			if ctx != nil && *ctx != nil {
				ctxVal = *ctx
			} else {
				ctxVal = context.Background()
			}
			if h.meter != nil {
				h.activeSpansGauge.Add(ctxVal, 1)
			}
			// non rilascio il lock qui: procedo a impostare policy e timer sotto lo stesso lock
			break
		}
		// collision: rilascio lock e riprovo
		h.mu.Unlock()
	}

	// Ora siamo fuori dal loop e il lock è tenuto: impostiamo le policy e il timer in modo atomico
	// Memorizzo la policy per questo span (nil => timeout disabilitato)
	// (NOTA: il lock è già acquisito qui)
	h.perSpanTimeouts[tidStr] = timeout
	// memorizzo minLevel se fornito
	if minLevel != nil {
		h.perSpanMinLevels[tidStr] = minLevel
	}
	// memorizzo tags convertiti in slog.Attr
	if len(tags) > 0 {
		attrs := make([]slog.Attr, 0, len(tags))
		for i, tg := range tags {
			attrs = append(attrs, slog.String(fmt.Sprintf("tag.%d", i), tg))
		}
		h.perSpanTags[tidStr] = attrs
	}
	// Gestiamo il timer associato in base alla policy
	if timeout == nil {
		// Se timeout è esplicitamente nil, intendiamo DISABILITARE il timer per questo span.
		// Rimuoviamo eventuali timer esistenti per sicurezza, ma non creiamo timer fittizi.
		if t, ok := h.timers[tidStr]; ok {
			t.Stop()
			delete(h.timers, tidStr)
		}
		// rilascio lock
		h.mu.Unlock()
	} else {
		// calcoliamo la durata effettiva: 0 nel puntatore significa usare il default dell'handler
		dur := h.timeoutDuration
		if *timeout > 0 {
			dur = *timeout
		}
		if t, ok := h.timers[tidStr]; ok {
			t.Reset(dur)
		} else {
			tr := tidStr
			h.timers[tr] = time.AfterFunc(dur, func() {
				select {
				case h.channel <- LogCommand{Op: OpTimeout, Context: context.Background(), TraceID: tr}:
				default:
				}
			})
		}
		// rilascio lock
		h.mu.Unlock()
	}

	// costruisco uno SpanContext usando il traceID generato e lo inserisco nel context passato
	if ctx == nil {
		return "", fmt.Errorf("nil context pointer")
	}
	// Genero anche uno SpanID non-zero così che lo SpanContext sia considerato valido
	var sid trace.SpanID
	if _, err := rand.Read(sid[:]); err != nil {
		// fallback deterministico
		now := time.Now().UnixNano()
		// copy first 8 bytes of the hex representation
		hex := fmt.Sprintf("%016x", now)
		copy(sid[:], []byte(hex)[:8])
	}
	var baseCtx context.Context
	if *ctx != nil {
		baseCtx = *ctx
	} else {
		baseCtx = context.Background()
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	*ctx = trace.ContextWithSpanContext(baseCtx, sc)
	return tidStr, nil
}

// SetSpanMinLevel imposta (o rimuove quando level == nil) il livello minimo di log
// che verrà bufferizzato/inoltrato per il traceID specificato.
// - level == nil => rimuove il filtro per quel traceID (accetta tutti i livelli)
// - level != nil => solo record con Record.Level >= *level verranno considerati
func (h *AsyncBufferedHandler) SetSpanMinLevel(traceID string, level *slog.Level) {
	if traceID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if level == nil {
		delete(h.perSpanMinLevels, traceID)
		return
	}
	h.perSpanMinLevels[traceID] = level
}

// Close chiude il canale dei comandi e attende che il worker termini.
func (h *AsyncBufferedHandler) Close() {
	h.closeOnce.Do(func() {
		atomic.StoreInt32(h.closed, 1)
		// chiudo il canale: i produttori dovrebbero controllare il flag closed prima di inviare
		close(h.channel)
	})
	h.wg.Wait()
}

// startWorker avvia un goroutine che ascolta il canale dei comandi di log
func (h *AsyncBufferedHandler) startWorker() {
	// Loop infinito per processare i comandi di log
	for cmd := range h.channel {
		h.process(cmd)
	}
}

// process gestisce i comandi di log in base al tipo di operazione
// OpReleaseSuccess: gestisce il rilascio delle risorse in caso di successo senza invio di log
// OpReleaseError: gestisce il rilascio delle risorse in caso di errore, forzando il flush dei log
// OpTimeout: gestisce il timeout, forzando il flush dei log
// OpLog: gestisce l'aggiunta di un record di log al buffer
func (h *AsyncBufferedHandler) process(cmd LogCommand) {
	// Non teniamo il lock per tutta la durata di process: le funzioni chiamate
	// (handleLogRecord, flush, cleanup) si occupano di lockare internamente
	switch cmd.Op {

	case OpReleaseSuccess:
		h.processOpReleaseSuccess(cmd)

	case OpReleaseError:
		h.processOpReleaseError(cmd)

	case OpTimeout:
		h.processOpTimeout(cmd)

	case OpLog:
		// Chiamo il metodo per gestire il log
		h.handleLogRecord(cmd)
	}
}

// processOpReleaseSuccess gestisce il rilascio delle risorse in caso di successo senza invio di log
func (h *AsyncBufferedHandler) processOpReleaseSuccess(cmd LogCommand) {
	// Controlle se il meter è impostato
	if h.meter != nil {
		// Aggiorno le metriche
		h.successCounter.Add(cmd.Context, 1)
		h.totalCounter.Add(cmd.Context, 1, metric.WithAttributes(attribute.String("status", "success")))
		// Chiamo il metodo per gestire il rilascio delle risorse
	}
	// Chiamo il metodo per pulire le risorse senza scrivere i log (in ogni caso di successo)
	h.cleanup(cmd.Context, cmd.TraceID)
}

// processOpReleaseError gestisce il rilascio delle risorse in caso di errore, forzando il flush dei log
func (h *AsyncBufferedHandler) processOpReleaseError(cmd LogCommand) {
	// Se è presente un errore, lo aggiungiamo al buffer come record di errore
	if cmd.Err != nil {
		r := slog.NewRecord(time.Now(), slog.LevelError, "OpReleaseErr", 0)
		r.AddAttrs(slog.String("err", cmd.Err.Error()))
		// Aggiungiamo il record al buffer in modo thread-safe
		h.mu.Lock()
		// assicurati che esista un timer per il traceID, ma rispetta la policy per-span se presente
		if _, exists := h.timers[cmd.TraceID]; !exists {
			// Verifichiamo se esiste una policy per questo span
			if policy, has := h.perSpanTimeouts[cmd.TraceID]; has {
				if policy != nil {
					// policy non-nil: usare la durata indicata (0 => handler default)
					d := h.timeoutDuration
					if *policy > 0 {
						d = *policy
					}
					tr := cmd.TraceID
					h.timers[tr] = time.AfterFunc(d, func() {
						select {
						case h.channel <- LogCommand{Op: OpTimeout, Context: context.Background(), TraceID: tr}:
						default:
						}
					})
				}
				// se policy == nil => timeout disabilitato -> non creare timer
			} else {
				// nessuna policy specifica -> comportamento di default
				tr := cmd.TraceID
				h.timers[tr] = time.AfterFunc(h.timeoutDuration, func() {
					select {
					case h.channel <- LogCommand{Op: OpTimeout, Context: context.Background(), TraceID: tr}:
					default:
					}
				})
			}
		}
		// bufferizzo il record di errore
		h.buffers[cmd.TraceID] = append(h.buffers[cmd.TraceID], r)
		if h.meter != nil {
			// eseguito solo se il meter è impostato
			h.activeRecordsGauge.Add(cmd.Context, 1)
		}
		h.mu.Unlock()
	}
	// Chiamo il metodo per scrivere i log e pulire le risorse
	h.flush(cmd.Context, cmd.TraceID)
}

func (h *AsyncBufferedHandler) processOpTimeout(cmd LogCommand) {
	if h.meter != nil {
		// Eseguito solo se il meter è impostato
		// Aggiorno le metriche
		h.errorCounter.Add(cmd.Context, 1, metric.WithAttributes(attribute.String("cause", "timeout")))
		h.totalCounter.Add(cmd.Context, 1, metric.WithAttributes(attribute.String("status", "timeout"), attribute.String("op", "timeout")))
	}
	// Chiamo il metodo per scrivere i log e pulire le risorse
	h.flush(cmd.Context, cmd.TraceID)
}

// Handle implementa l'interfaccia slog.Handler
// Prende in input un contesto e un record di log
// e tenta di inviare un comando di log al canale
// Se il canale è pieno, scarta il log e incrementa anche il contatore dei drop
func (h *AsyncBufferedHandler) Handle(ctx context.Context, record slog.Record) error {
	// se l'handler è stato chiuso, scartiamo i record
	if atomic.LoadInt32(h.closed) == 1 {
		return nil
	}
	select {
	case h.channel <- LogCommand{
		Op:      OpLog,
		Record:  record,
		Context: ctx,
		Tags:    h.attrs,
	}:
	default:
		// Canale pieno
		h.handleDrop(ctx)
	}
	return nil
}

// handleDrop viene chiamato quando un log viene scartato a causa del canale pieno
func (h *AsyncBufferedHandler) handleDrop(ctx context.Context) {
	if h.meter != nil {
		// Eseguito solo se il meter è impostato
		h.dropCounter.Add(ctx, 1)
		h.totalCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "dropped")))
	}
}

// Enabled implementa l'interfaccia slog.Handler
// Verifica se il livello di log è abilitato nel gestore successivo
func (h *AsyncBufferedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// WithAttrs restituisce un nuovo gestore con attributi aggiuntivi a quelli forniti
// Utile per mantenere il contesto dei log
func (h *AsyncBufferedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	// Preparo la lista di attributi da usare
	newTotalAttrs := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	newTotalAttrs = append(newTotalAttrs, h.attrs...)
	newTotalAttrs = append(newTotalAttrs, attrs...)

	// Costruisco un nuovo handler riutilizzando le risorse condivise (mutex, mappe, metriche)
	return &AsyncBufferedHandler{
		next:             h.next.WithAttrs(attrs),
		channel:          h.channel,
		mu:               h.mu,
		buffers:          h.buffers,
		timers:           h.timers,
		perSpanTimeouts:  h.perSpanTimeouts,
		perSpanMinLevels: h.perSpanMinLevels, // propagato
		perSpanTags:      h.perSpanTags,      // propagato
		timeoutDuration:  h.timeoutDuration,
		attrs:            newTotalAttrs,
		meter:            h.meter,
		// metriche
		totalCounter:       h.totalCounter,
		successCounter:     h.successCounter,
		errorCounter:       h.errorCounter,
		dropCounter:        h.dropCounter,
		activeSpansGauge:   h.activeSpansGauge,
		activeRecordsGauge: h.activeRecordsGauge,
		wg:                 h.wg,
		closeOnce:          h.closeOnce,
		closed:             h.closed,
	}
}

func (h *AsyncBufferedHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	// Costruisco un nuovo handler riutilizzando le risorse condivise (mutex, mappe, metriche)
	return &AsyncBufferedHandler{
		next:             h.next.WithGroup(name),
		channel:          h.channel,
		mu:               h.mu,
		buffers:          h.buffers,
		timers:           h.timers,
		perSpanTimeouts:  h.perSpanTimeouts,
		perSpanMinLevels: h.perSpanMinLevels, // propagato
		perSpanTags:      h.perSpanTags,      // propagato
		timeoutDuration:  h.timeoutDuration,
		attrs:            h.attrs,
		meter:            h.meter,
		// metriche
		totalCounter:       h.totalCounter,
		successCounter:     h.successCounter,
		errorCounter:       h.errorCounter,
		dropCounter:        h.dropCounter,
		activeSpansGauge:   h.activeSpansGauge,
		activeRecordsGauge: h.activeRecordsGauge,
		wg:                 h.wg,
		closeOnce:          h.closeOnce,
		closed:             h.closed,
	}
}

// handleLogRecord gestisce l'aggiunta di un record di log al buffer
func (h *AsyncBufferedHandler) handleLogRecord(cmd LogCommand) {
	tID := cmd.TraceID
	if tID == "" {
		// Se tID è vuoto
		// Creo un nuovo traceID dal contesto
		s := trace.SpanContextFromContext(cmd.Context)
		if s.IsValid() {
			tID = s.TraceID().String()
		}
	}
	if tID == "" {
		// Se ancora tID è vuoto
		// aggiungo gli attributi e passo il log al gestore successivo
		cmd.Record.AddAttrs(cmd.Tags...)
		_ = h.next.Handle(cmd.Context, cmd.Record)
		return
	}

	// Controllo per-span minimum log level: se impostato e il livello del record è minore => scartalo
	h.mu.Lock()
	if minPtr, ok := h.perSpanMinLevels[tID]; ok && minPtr != nil {
		minLevel := *minPtr
		if cmd.Record.Level < minLevel {
			// Non bufferizziamo né inoltriamo il record
			h.mu.Unlock()
			return
		}
	}
	// Se è una nuova traccia, avvio un timer per il timeout e incremento il gauge degli span attivi
	if _, exists := h.buffers[tID]; !exists {
		// nuovo buffer
		if h.meter != nil {
			// eseguito solo se il meter è impostato
			// incremento gli span attivi
			h.activeSpansGauge.Add(cmd.Context, 1)
		}
	}
	// se non esiste un timer, ne creo uno rispettando la policy per-span (se presente)
	if _, exists := h.timers[tID]; !exists {
		// controlliamo se esiste una politica per questo span
		if policy, has := h.perSpanTimeouts[tID]; has {
			if policy != nil {
				// policy non-nil: 0 => default handler timeout; >0 => custom timeout
				dur := h.timeoutDuration
				if *policy > 0 {
					dur = *policy
				}
				tr := tID
				h.timers[tr] = time.AfterFunc(dur, func() {
					select {
					case h.channel <- LogCommand{Op: OpTimeout, Context: context.Background(), TraceID: tr}:
					default:
					}
				})
			}
			// se policy == nil -> timeout disabilitato: non creare timer
		} else {
			// nessuna policy specifica => comportamento di default
			tr := tID
			h.timers[tr] = time.AfterFunc(h.timeoutDuration, func() {
				select {
				case h.channel <- LogCommand{Op: OpTimeout, Context: context.Background(), TraceID: tr}:
				default:
				}
			})
		}
	}
	// Aggiungo gli attributi al record di log: prima quelli globali
	cmd.Record.AddAttrs(cmd.Tags...)
	// Aggiungo anche i tag specifici dello span se presenti
	if spanAttrs, ok := h.perSpanTags[tID]; ok && len(spanAttrs) > 0 {
		cmd.Record.AddAttrs(spanAttrs...)
	}
	// Aggiungo il record al buffer
	h.buffers[tID] = append(h.buffers[tID], cmd.Record)
	if h.meter != nil {
		// eseguito solo se il meter è impostato
		// incremento il contatore dei record attivi
		h.activeRecordsGauge.Add(cmd.Context, 1)
	}
	// Determino se serve un flush immediato
	forceFlush := cmd.Record.Level >= slog.LevelError
	h.mu.Unlock()

	if forceFlush {
		if h.meter != nil {
			// Eseguito solo se il meter è impostato
			// Aggiorno le metriche
			h.errorCounter.Add(cmd.Context, 1, metric.WithAttributes(attribute.String("cause", "immediate_flush")))
			h.totalCounter.Add(cmd.Context, 1, metric.WithAttributes(attribute.String("status", "immediate_flush")))
		}
		h.flush(cmd.Context, tID)
	}
}

// flush scrive i log bufferizzati per un dato traceID e pulisce le risorse
func (h *AsyncBufferedHandler) flush(ctx context.Context, traceID string) {
	h.mu.Lock()
	recs, ok := h.buffers[traceID]
	if ok {
		// Aggiorno i contatori e rimuovo stato sotto lock
		delete(h.buffers, traceID)
		if t, tok := h.timers[traceID]; tok {
			// fermo il timer
			t.Stop()
			delete(h.timers, traceID)
		}
		// Rimuovo anche la policy per-span associata, se presente
		delete(h.perSpanTimeouts, traceID)
		// Rimuovo anche la policy min-level per-span, se presente
		delete(h.perSpanMinLevels, traceID)
		// Rimuovo anche i tag per-span
		delete(h.perSpanTags, traceID)
		if h.meter != nil {
			// Eseguito solo se il meter è impostato
			// Aggiorno il contatore dei record attivi
			h.activeRecordsGauge.Add(ctx, int64(-len(recs)))
			// Aggiorno il contatore degli span attivi
			h.activeSpansGauge.Add(ctx, -1)
		}
	}
	h.mu.Unlock()

	if !ok {
		return
	}

	for _, r := range recs {
		_ = h.next.Handle(ctx, r)
	}
}

// cleanup pulisce le risorse senza scrivere i log per un dato traceID
func (h *AsyncBufferedHandler) cleanup(ctx context.Context, traceID string) {
	h.mu.Lock()
	if recs, ok := h.buffers[traceID]; ok {
		if h.meter != nil {
			// Eseguito solo se il meter è impostato
			// Aggiorno il contatore dei record attivi
			h.activeRecordsGauge.Add(ctx, int64(-len(recs)))
			// Aggiorno il contatore degli span attivi
			h.activeSpansGauge.Add(ctx, -1)
		}

		// Rimuovo il buffer e il timer
		delete(h.buffers, traceID)
		if t, tok := h.timers[traceID]; tok {
			t.Stop()
			delete(h.timers, traceID)
		}
		// Rimuovo anche la policy per-span
		delete(h.perSpanTimeouts, traceID)
		// Rimuovo anche la policy min-level per-span
		delete(h.perSpanMinLevels, traceID)
		// Rimuovo anche i tag per-span
		delete(h.perSpanTags, traceID)
	}
	h.mu.Unlock()
}

//  ---- IMPLEMENTAZIONE DELL'INTERFACCIA SpanLogger -----

// EndSpanSuccess segnala la fine di uno span con successo
func (h *AsyncBufferedHandler) EndSpanSuccess(ctx context.Context, traceID string) {
	// Eseguiamo il cleanup sincrono per evitare condizioni di race tra il
	// timer (OpTimeout) e l'invio asincrono del comando OpReleaseSuccess.
	// Stoppiamo il timer, rimuoviamo eventuali record dal buffer e aggiorniamo
	// i contatori sotto lock.
	h.mu.Lock()
	if t, ok := h.timers[traceID]; ok {
		t.Stop()
		delete(h.timers, traceID)
	}
	if recs, ok := h.buffers[traceID]; ok {
		if h.meter != nil {
			// Eseguito solo se il meter è impostato
			// Aggiorno i contatori
			h.activeRecordsGauge.Add(ctx, int64(-len(recs)))
			h.activeSpansGauge.Add(ctx, -1)
		}

		// Rimuovo il buffer
		delete(h.buffers, traceID)
	}
	// Rimuovo anche la policy per-span associata
	delete(h.perSpanTimeouts, traceID)
	// Rimuovo anche la policy min-level per-span
	delete(h.perSpanMinLevels, traceID)
	// Rimuovo anche i tag per-span
	delete(h.perSpanTags, traceID)
	h.mu.Unlock()

	if h.meter != nil {
		// Aggiorno le metriche di successo (come faceva OpReleaseSuccess)
		h.successCounter.Add(ctx, 1)
		h.totalCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
	}
}

// EndSpanError segnala la fine di uno span con errore
func (h *AsyncBufferedHandler) EndSpanError(ctx context.Context, traceID string, err error) {
	h.sendControl(ctx, traceID, OpReleaseError, err)
}

// sendControl invia un comando di controllo al canale
func (h *AsyncBufferedHandler) sendControl(ctx context.Context, traceID string, op OpType, err error) {
	// non inviamo se l'handler è stato chiuso
	if atomic.LoadInt32(h.closed) == 1 {
		return
	}
	select {
	case h.channel <- LogCommand{
		Op:      op,
		Context: ctx,
		TraceID: traceID,
		Err:     err,
	}:
	default:
	}
}
