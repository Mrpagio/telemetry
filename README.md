README - telemetry (libreria di logging bufferizzato e asincrono)

Lingua: Italiano

Scopo
-----
Questo repository espone un handler di logging asincrono e bufferizzato che raccoglie record per span (traceID) e li flusha al gestore "next" in caso di errore, di timeout o quando lo span termina con errore.

Il package principale utilizzabile da altri progetti è: github.com/Mrpagio/telemetry/pkg

Modifiche e funzionalità principali
----------------------------------
La libreria implementa un handler `AsyncBufferedHandler` che fornisce le seguenti funzionalità (inclusi i nuovi comportamenti aggiunti):

- Buffering per traceID: i record con contesto contenente uno SpanContext valido vengono bufferizzati per trace (traceID) invece di essere inoltrati immediatamente.
- Flush on error: se arriva un record con livello >= ERROR il buffer relativo allo span viene flushato immediatamente.
- Timeout per span: i buffer configurati vengono flusshati automaticamente dopo un timeout. Sono disponibili policy per-span per disabilitare o personalizzare il timeout.
- Per-span timeout policy (`StartSpan`): è possibile impostare una policy di timeout per uno specifico span quando si crea lo span tramite `StartSpan`.
  - Nota: la funzione `StartSpan` ora genera internamente un `traceID` unico e scrive lo `SpanContext` nel `context` fornito (vedi dettagli sulla firma più avanti).
  - `timeout` come *pointer* a `time.Duration` supporta tre comportamenti:
    - `timeout == nil` => timeout DISABILITATO per quello span (nessun flush automatico per timeout);
    - `timeout != nil && *timeout == 0` => usare il `timeout` di default dell'handler;
    - `timeout != nil && *timeout > 0` => usare la durata specificata per quello span.
- Per-span minimum log level: è possibile impostare un livello minimo di log per uno span specifico in due modi:
  - passando `minLevel` a `StartSpan` (opzionale) — il filtro sarà applicato allo span creato;
  - oppure chiamando `SetSpanMinLevel(traceID, level *slog.Level)` in un momento successivo.
  - In entrambi i casi, se il valore è `nil` il filtro viene rimosso (accettati tutti i livelli).
- API per la chiusura e per la fine degli span:
  - `EndSpanSuccess(ctx, traceID)` — termina lo span con successo e pulisce lo stato senza flushare i log bufferizzati;
  - `EndSpanError(ctx, traceID, err)` — termina lo span con errore, aggiunge un record di errore e forza il flush del buffer.
- Propagazione delle risorse in `WithAttrs`/`WithGroup`: i metodi `WithAttrs` e `WithGroup` restituiscono un handler che condivide lo stato interno (buffer, timer, policy) e propagano anche le nuove mappe di policy per-span.
- Metriche (opzionali): l'handler può ricevere un `meter` (interfaccia minima `MyMeter`) per registrare metriche OTel fake/real come contatori e gauge (totale, successi, errori, drop, span attivi, record attivi). È possibile passare `nil` per disabilitare le metriche.

Nota sui nomi: il package usa internamente mappe denominate `perSpanTimeouts` e `perSpanMinLevels` per memorizzare le policy per traceID.

Dichiarazione dei tipi (sintesi aggiornata)
-------------------------------------------
Queste sono le callable principali e i tipi di interesse definiti in `pkg/logger.go` (elenco sintetico aggiornato):

- func NewAsyncBufferedHandler(next slog.Handler, meter MyMeter, bufferSize int, timeout time.Duration) *AsyncBufferedHandler
- (h *AsyncBufferedHandler) Handle(ctx context.Context, record slog.Record) error
- (h *AsyncBufferedHandler) Enabled(ctx context.Context, level slog.Level) bool
- (h *AsyncBufferedHandler) WithAttrs(attrs []slog.Attr) slog.Handler
- (h *AsyncBufferedHandler) WithGroup(name string) slog.Handler
- (h *AsyncBufferedHandler) StartSpan(ctx *context.Context, timeout *time.Duration, minLevel *slog.Level, tags []string) (string, error)
  - Genera un nuovo `traceID` univoco, inizializza lo stato interno (buffer, policy) per quello span e scrive lo `SpanContext` nel `context` passato come puntatore (`*context.Context`). Restituisce il `traceID` generato o un errore (ad esempio se il puntatore ctx è nil).
- (h *AsyncBufferedHandler) SetSpanMinLevel(traceID string, level *slog.Level)
  - Imposta il livello minimo di log per traceID; `level == nil` rimuove il filtro.
- (h *AsyncBufferedHandler) EndSpanSuccess(ctx context.Context, traceID string)
- (h *AsyncBufferedHandler) EndSpanError(ctx context.Context, traceID string, err error)
- (h *AsyncBufferedHandler) Close()

Esempi d'uso aggiornati
-----------------------
1) Esempio minimo con un `fake meter` (utile per sviluppare/testare):

```go
package main

import (
    "context"
    "os"
    "time"

    "log/slog"
    "github.com/Mrpagio/telemetry/pkg"
)

func main() {
    // next handler che scrive JSON su stdout
    next := slog.NewJSONHandler(os.Stdout, nil)

    // puoi passare nil per disabilitare le metriche (o usare il fakeMeter presente nei test)
    var fm pkg.MyMeter = nil

    // costruttore: next handler, meter (opzionale), bufferSize, default timeout
    h := pkg.NewAsyncBufferedHandler(next, fm, 100, 200*time.Millisecond)
    defer h.Close()

    logger := slog.New(h)

    // log senza trace -> inoltro immediato
    logger.Info("hello world")

    // Per usare le policy per-span: creare un context con SpanContext contenente un traceID
    // (nella pratica questo viene normalmente fornito dall'instrumentazione OpenTelemetry)
    // Esempio illustrativo: iniziamo da un context vuoto e chiediamo all'handler di creare lo span
    ctx := context.Background()

    // Esempio: start span con timeout personalizzato e filtro min-level WARN
    custom := 500 * time.Millisecond
    lvl := slog.LevelWarn
    traceID, err := h.StartSpan(&ctx, &custom, &lvl, []string{"http.route", "user"})
    if err != nil {
        // gestire l'errore (ad esempio panic o log)
        panic(err)
    }
    // ora `ctx` contiene lo SpanContext inserito e puoi passarlo a logger.Handle per associare i log a questo span
    logger.InfoContext(ctx, "request started")

    // quando lo span termina con successo (senza voler forwardare i log bufferizzati):
    h.EndSpanSuccess(ctx, traceID)

    // se lo span termina con errore e vogliamo forzare il flush dei log bufferizzati:
    // h.EndSpanError(ctx, traceID, errors.New("something went wrong"))
}
```

Note sull'uso di `StartSpan` e sul contesto
-----------------------------------------
- `StartSpan` ora riceve un puntatore a `context.Context` (`*context.Context`) e scrive il `SpanContext` creato dentro la variabile puntata (cioè fa `*ctx = trace.ContextWithSpanContext(...)`). Questo rende facile continuare a usare la stessa variabile `ctx` aggiornata dopo la chiamata.
- Se passi `nil` come puntatore (`h.StartSpan(nil, ...)`) la funzione ritorna un errore: è necessario passare l'indirizzo di una variabile `context.Context`.
- La funzione genera internamente un `traceID` (non lo riceve in ingresso) e lo restituisce: puoi usarlo con `SetSpanMinLevel`, `EndSpanSuccess`, `EndSpanError`.

2) Esempio rapido per l'uso in un altro modulo (go.mod)

Se la libreria è pubblicata su GitHub sotto `github.com/Mrpagio/telemetry`, nel tuo `go.mod` del progetto consumer aggiungi:

    require github.com/Mrpagio/telemetry v0.1.1

(per sviluppo locale puoi usare una direttiva `replace` per puntare alla copia locale del repository):

    replace github.com/Mrpagio/telemetry => ../path/to/local/telemetry

Dopo aver aggiunto la dipendenza, importa il package:

    import "github.com/Mrpagio/telemetry/pkg"

Note sui test e sugli strumenti di sviluppo
-----------------------------------------
Nel repository sono presenti test che usano due helper utili per simulare il comportamento delle metriche e del gestore finale:

- `fakeMeter` — implementa l'interfaccia minima `MyMeter` per creare strumenti finti e tracciare le chiamate alle API di metriche;
- `fakeSlogHandler` — un handler che cattura i `slog.Record` inoltrati e permette di sincronizzare i test con `sync.WaitGroup`.

Esempi di comandi utili per lo sviluppo e i test:

```bash
# esegui tutti i test del pacchetto pkg
go test ./pkg -v

# esegui un singolo test
go test -run TestPerSpanMinLevelFiltering -v ./pkg

# esegui i test con il rilevamento delle data race
go test -race ./pkg -v
```

Suggerimenti e possibili miglioramenti futuri
--------------------------------------------
- Fornire un adattatore ufficiale per `otel.Meter` in modo da poter passare un `metric.Meter` reale all'handler invece dell'interfaccia minima `MyMeter`.
- Esportare helper per costruire facilmente SpanContext/traceID nei consumer degli esempi.
- Aggiungere esempi più completi che mostrino l'integrazione con OpenTelemetry (strumentazione di tracer e propagazione dei contesti).
