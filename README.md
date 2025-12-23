README - telemetry (libreria di logging bufferizzato e asincrono)

Lingua: Italiano

Scopo
-----
Questo repository espone un handler di logging asincrono e bufferizzato che raccoglie record per span (traceID) e li flusha al gestore "next" in caso di errore o timeout.

Il package principale utilizzabile da altri progetti è: github.com/jacopomanzoli/telemetry/pkg

Modifica per uso come libreria
-----------------------------
Ho aggiornato `go.mod` impostando il module path su:

    module github.com/jacopomanzoli/telemetry

Questo rende i pacchetti importabili come `github.com/jacopomanzoli/telemetry/pkg`.

Dichiarazione dei tipi (esclusi i test)
--------------------------------------
Qui sotto trovi una lista sintetica dei tipi esportati (e non) definiti in `pkg/logger.go`.

- type SpanLogger interface
  - EndSpanSuccess(ctx context.Context, traceID string)
  - EndSpanError(ctx context.Context, traceID string, err error)

- type Int64CounterLike interface
  - (metodo) Add(ctx context.Context, value int64, opts ...metric.AddOption)

- type Int64UpDownCounterLike interface
  - (metodo) Add(ctx context.Context, value int64, opts ...metric.AddOption)

- type FakeMeter interface
  - Int64Counter(name string, opts ...metric.InstrumentOption) (Int64CounterLike, error)
  - Int64UpDownCounter(name string, opts ...metric.InstrumentOption) (Int64UpDownCounterLike, error)

- type OpType (enum: OpLog, OpReleaseSuccess, OpReleaseError, OpTimeout)

- type LogCommand struct {
    Op OpType
    Record slog.Record
    Context context.Context
    TraceID string
    Tags []slog.Attr
    Err error
  }

- type AsyncBufferedHandler struct { /* handler interno con campi */ }
  - implementa metodi (vedi sotto)

Dichiarazione delle funzioni / metodi pubblici (esclusi i test)
----------------------------------------------------------------
Queste sono le callable principali che userai quando importi il pacchetto.

- func NewAsyncBufferedHandler(next slog.Handler, meter FakeMeter, bufferSize int, timeout time.Duration) *AsyncBufferedHandler
  - Costruisce un nuovo handler asincrono. `next` è il handler chiamato per i record effettivamente flushati.
  - `meter` deve corrispondere all'interfaccia `FakeMeter` (in produzione puoi passare un adattatore che richiami `metric.Meter`).

- (h *AsyncBufferedHandler) Handle(ctx context.Context, record slog.Record) error
  - Implementa `slog.Handler`. Invia il record al buffer/worker asincrono.

- (h *AsyncBufferedHandler) Enabled(ctx context.Context, level slog.Level) bool
  - Propaga la chiamata al `next` handler.

- (h *AsyncBufferedHandler) WithAttrs(attrs []slog.Attr) slog.Handler
  - Restituisce un handler con attributi aggiuntivi (condivide lo stato interno per buffering/timer).

- (h *AsyncBufferedHandler) WithGroup(name string) slog.Handler
  - Restituisce un handler che incapsula i futuri log nel gruppo specificato.

- (h *AsyncBufferedHandler) EndSpanSuccess(ctx context.Context, traceID string)
  - Segnala la fine dello span con successo: pulisce lo stato del buffer relativo al traceID senza flushare i log.

- (h *AsyncBufferedHandler) EndSpanError(ctx context.Context, traceID string, err error)
  - Segnala la fine dello span con errore: invia un comando che aggiunge un record di errore e flusha.

- (h *AsyncBufferedHandler) Close()
  - Chiude il canale interno e attende che il worker termini. Usare per pulire risorse nei test o all'arresto dell'app.

Nota: sono presenti ulteriori metodi non esportati (handleLogRecord, flush, cleanup, sendControl) che gestiscono la logica interna.

Esempi d'uso
-------------
1) Esempio minimo con un `fake meter` (utile per sviluppare/testare):

```go
package main

import (
    "context"
    "time"

    "log/slog"
    "github.com/Mrpagio/telemetry/pkg"
)

// implementa una FakeMeter minimale (puoi riusare il fake del repo)
// qui usiamo il fake presente nei test come esempio.

func main() {
    // next handler che stampa su stdout
    next := slog.NewTextHandler(os.Stdout, nil)

    // esempio di FakeMeter: nei test del repo è definito `fakeMeter`.
    fm := pkg.NewFakeMeterForExample() // vedi nota sotto — o implementa il tuo adattatore

    h := pkg.NewAsyncBufferedHandler(next, fm, 100, 200*time.Millisecond)
    defer h.Close()

    logger := slog.New(h)

    // log senza trace -> inoltro immediato
    logger.Info("hello world")

    // Con un span/trace (esempio): creare un context con span context e quindi loggare
    // se il record è d'errore, verrà flushato immediatamente; altrimenti verrà bufferizzato
}
```

Nota: Nel repository di esempio i test definiscono un `fakeMeter` e un `fakeSlogHandler` che puoi riutilizzare come base per costruire un adattatore di `metric.Meter` reale o per testare localmente.

2) Esempio per importare in un altro modulo (go.mod):

Se la libreria è pubblicata su GitHub sotto `github.com/jacopomanzoli/telemetry`, nel tuo `go.mod` del progetto consumer aggiungi:

    require github.com/jacopomanzoli/telemetry v0.0.0

(per sviluppo locale puoi usare una direttiva `replace` per puntare alla copia locale del repository):

    replace github.com/jacopomanzoli/telemetry => ../path/to/local/telemetry

Dopo aver aggiunto la dipendenza, importa il package:

    import "github.com/jacopomanzoli/telemetry/pkg"

Suggerimenti e next steps
-------------------------
- Se vuoi usare la libreria con un `metric.Meter` reale, scrivi un piccolo adattatore che soddisfi l'interfaccia `FakeMeter` usando `otel.Meter(...)` e gli strumenti concreti. Questo evita di dover cambiare l'API del package.
- Nei test del repo trovi un `fakeMeter` e `fakeSlogHandler` che sono un buon punto di partenza per testare integrazioni.
- Per eseguire i test del pacchetto:

```bash
# esegui tutti i test (pacchetto pkg)
go test ./pkg -v

# esegui singolo test
go test -run TestBufferingEndSpanSuccess -v ./pkg

# con rilevamento delle data race
go test -race ./pkg -v
```


