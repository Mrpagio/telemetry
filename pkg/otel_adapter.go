package pkg

import (
	"go.opentelemetry.io/otel/metric"
)

// MeterAdapter adatta un metric.Meter reale all'interfaccia FakeMeter usata dal package.
// Questo permette a chi importa il package di passare un meter reale senza cambiare API.
type MeterAdapter struct {
	m metric.Meter
}

// NewMeterAdapter ritorna un adattatore che implementa FakeMeter
func NewMeterAdapter(m metric.Meter) *MeterAdapter {
	return &MeterAdapter{m: m}
}

func (a *MeterAdapter) Int64Counter(name string, _ ...metric.InstrumentOption) (Int64CounterLike, error) {
	// Non inoltriamo le option perché le API concrete del Meter possono aspettare
	// opzioni di tipo diverso (es. instrument.Int64CounterOption). Chi ha bisogno
	// di configurare le instrument deve creare l'adapter con lo strumento già
	// configurato.
	inst, err := a.m.Int64Counter(name)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

func (a *MeterAdapter) Int64UpDownCounter(name string, _ ...metric.InstrumentOption) (Int64UpDownCounterLike, error) {
	inst, err := a.m.Int64UpDownCounter(name)
	if err != nil {
		return nil, err
	}
	return inst, nil
}
