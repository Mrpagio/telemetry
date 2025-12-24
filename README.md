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
- Per-span timeout policy (`StartSpan`): è possibile impostare una policy di timeout per uno specifico traceID tramite `StartSpan(ctx, traceID, timeout *time.Duration)`. Il comportamento supportato è:
  - `timeout == nil` => timeout DISABILITATO per quello span (nessun flush automatico per timeout);
  - `timeout != nil && *timeout == 0` => usare il `timeout` di default dell'handler;
  - `timeout != nil && *timeout > 0` => usare la durata specificata per quello span.
- Per-span minimum log level (`SetSpanMinLevel`): è possibile imporre un livello minimo di log per uno span specifico con `SetSpanMinLevel(traceID, level *slog.Level)`. Se impostato, i record con livello inferiore verranno scartati per quello span. Passando `nil` si rimuove il filtro.
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
- (h *AsyncBufferedHandler) StartSpan(ctx context.Context, traceID string, timeout *time.Duration)
  - Inizializza/aggiorna il buffer e la policy di timeout per lo span; `timeout == nil` disabilita il timeout per quello span.
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
    // Esempio illustrativo: recupera traceID da un context con SpanContext
    ctx := context.Background()

    // Esempio rapido: impostare un livello minimo WARN per uno span specifico
    traceID := "0123456789abcdef0123456789abcdef" // esempio
    lvl := slog.LevelWarn
    h.SetSpanMinLevel(traceID, &lvl)
    // per rimuovere il filtro:
    // h.SetSpanMinLevel(traceID, nil)

    // Esempio di start span con timeout personalizzato (nil per disabilitare timeout)
    custom := 500 * time.Millisecond
    h.StartSpan(ctx, traceID, &custom)
    // per disabilitare il timeout su questo span:
    // h.StartSpan(ctx, traceID, nil)

    // Quando lo span termina con successo (non vogliamo flushare i log bufferizzati):
    // h.EndSpanSuccess(ctx, traceID)

    // Se lo span termina con errore e vogliamo forzare il flush:
    // h.EndSpanError(ctx, traceID, errors.New("something went wrong"))
}
```

2) Esempio rapido per l'uso in un altro modulo (go.mod)

Se la libreria è pubblicata su GitHub sotto `github.com/Mrpagio/telemetry`, nel tuo `go.mod` del progetto consumer aggiungi:

    require github.com/Mrpagio/telemetry v0.0.7

(per sviluppo locale puoi usare una direttiva `replace` per puntare alla copia locale del repository):

    replace github.com/Mrpagio/telemetry => ../path/to/local/telemetry

Dopo aver aggiunto la dipendenza, importa il package:

    import "github.com/Mrpagio/telemetry/pkg"

Note sui test e sugli strumenti di sviluppo
-----------------------------------------
Nel repository sono presenti test che usano due helper utili per simulare il comportamento delle metriche e del gestore finale:

- `fakeMeter` — implementa l'interfaccia minimale `MyMeter` per creare strumenti finti e tracciare le chiamate alle API di metriche;
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
