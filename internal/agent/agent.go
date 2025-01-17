package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/xvzf/computeblade-agent/pkg/hal"
	"github.com/xvzf/computeblade-agent/pkg/ledengine"
	"github.com/xvzf/computeblade-agent/pkg/log"
	"go.uber.org/zap"
)

var (
	// eventCounter is a prometheus counter that counts the number of events handled by the agent
	eventCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "computeblade_agent",
		Name:      "events_count",
		Help:      "ComputeBlade Agent internal event handler statistics (handled events)",
	}, []string{"type"})

	// droppedEventCounter is a prometheus counter that counts the number of events dropped by the agent
	droppedEventCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "computeblade_agent",
		Name:      "events_dropped_count",
		Help:      "ComputeBlade Agent internal event handler statistics (dropped events)",
	}, []string{"type"})
)

type Event int

const (
	NoopEvent = iota
	IdentifyEvent
	IdentifyConfirmEvent
	CriticalEvent
	CriticalResetEvent
	EdgeButtonEvent
)

func (e Event) String() string {
	switch e {
	case NoopEvent:
		return "noop"
	case IdentifyEvent:
		return "identify"
	case IdentifyConfirmEvent:
		return "identify_confirm"
	case CriticalEvent:
		return "critical"
	case CriticalResetEvent:
		return "critical_reset"
	case EdgeButtonEvent:
		return "edge_button"
	default:
		return "unknown"
	}
}

type ComputeBladeAgentConfig struct {

	// IdleLedColor is the color of the edge LED when the blade is idle mode
	IdleLedColor hal.LedColor
	// IdentifyLedColor is the color of the edge LED when the blade is in identify mode
	IdentifyLedColor hal.LedColor
	// CriticalLedColor is the color of the top(!) LED when the blade is in critical mode.
	// In the circumstance when >1 blades are in critical mode, the identidy function can be used to find the right blade
	CriticalLedColor hal.LedColor

	// StealthModeEnabled indicates whether stealth mode is enabled
	StealthModeEnabled bool

	// DefaultFanSpeed is the default fan speed in percent. Usually 40% is sufficient
	DefaultFanSpeed uint

	// Critical temperature of the compute blade (used to trigger critical mode)
	CriticalTemperature uint
}

// ComputeBladeAgent implements the core-logic of the agent. It is responsible for handling events and interfacing with the hardware.
type ComputeBladeAgent interface {
	// Run dispatches the agent and blocks until the context is canceled or an error occurs
	Run(ctx context.Context) error
	// EmitEvent emits an event to the agent
	EmitEvent(ctx context.Context, event Event) error
	// SetFanSpeed sets the fan speed in percent
	SetFanSpeed(_ context.Context, speed uint8) error
	// SetStealthMode sets the stealth mode
	SetStealthMode(_ context.Context, enabled bool) error

	// WaitForIdentifyConfirm blocks until the user confirms the identify mode
	WaitForIdentifyConfirm(ctx context.Context) error
}

// computeBladeAgentImpl is the implementation of the ComputeBladeAgent interface
type computeBladeAgentImpl struct {
	opts          ComputeBladeAgentConfig
	blade         hal.ComputeBladeHal
	state         ComputebladeState
	edgeLedEngine ledengine.LedEngine
	topLedEngine  ledengine.LedEngine

	eventChan chan Event
}

func NewComputeBladeAgent(opts ComputeBladeAgentConfig) (ComputeBladeAgent, error) {
	var err error

	// blade, err := hal.NewCm4Hal(hal.ComputeBladeHalOpts{
	blade, err := hal.NewCm4Hal(hal.ComputeBladeHalOpts{
		FanUnit: hal.FanUnitStandard, // FIXME: support smart fan unit
	})
	if err != nil {
		return nil, err
	}

	edgeLedEngine := ledengine.NewLedEngine(ledengine.LedEngineOpts{
		LedIdx: hal.LedEdge,
		Hal:    blade,
	})
	if err != nil {
		return nil, err
	}

	topLedEngine := ledengine.NewLedEngine(ledengine.LedEngineOpts{
		LedIdx: hal.LedTop,
		Hal:    blade,
	})
	if err != nil {
		return nil, err
	}

	return &computeBladeAgentImpl{
		opts:          opts,
		blade:         blade,
		edgeLedEngine: edgeLedEngine,
		topLedEngine:  topLedEngine,
		state:         NewComputeBladeState(),
		eventChan:     make(chan Event, 10), // backlog of 10 events. They should process fast but we e.g. don't want to miss button presses
	}, nil
}

func (a *computeBladeAgentImpl) Run(origCtx context.Context) error {
	var wg sync.WaitGroup
	ctx, cancelCtx := context.WithCancelCause(origCtx)
	defer a.cleanup(ctx)

	log.FromContext(ctx).Info("Starting ComputeBlade agent")

	// Ingest noop event to initialise metrics
	a.state.RegisterEvent(NoopEvent)

	// Set defaults
	if err := a.blade.SetFanSpeed(uint8(a.opts.DefaultFanSpeed)); err != nil {
		return err
	}
	if err := a.blade.SetStealthMode(a.opts.StealthModeEnabled); err != nil {
		return err
	}

	// Start edge button event handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.FromContext(ctx).Info("Starting edge button event handler")
		for {
			err := a.blade.WaitForEdgeButtonPress(ctx)
			if err != nil && err != context.Canceled {
				log.FromContext(ctx).Error("Edge button event handler failed", zap.Error(err))
				cancelCtx(err)
			} else if err != nil {
				return
			}
			select {
			case a.eventChan <- Event(EdgeButtonEvent):
			default:
				log.FromContext(ctx).Warn("Edge button press event dropped due to backlog")
				droppedEventCounter.WithLabelValues(Event(EdgeButtonEvent).String()).Inc()
			}
		}
	}()

	// Start top LED engine
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.FromContext(ctx).Info("Starting top LED engine")
		err := a.runTopLedEngine(ctx)
		if err != nil && err != context.Canceled {
			log.FromContext(ctx).Error("Top LED engine failed", zap.Error(err))
			cancelCtx(err)
		}
	}()

	// Start edge LED engine
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.FromContext(ctx).Info("Starting edge LED engine")
		err := a.runEdgeLedEngine(ctx)
		if err != nil && err != context.Canceled {
			log.FromContext(ctx).Error("Edge LED engine failed", zap.Error(err))
			cancelCtx(err)
		}
	}()

	// Start event handler
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.FromContext(ctx).Info("Starting event handler")
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-a.eventChan:
				err := a.handleEvent(ctx, event)
				if err != nil && err != context.Canceled {
					log.FromContext(ctx).Error("Event handler failed", zap.Error(err))
					cancelCtx(err)
				}
			}
		}
	}()

	wg.Wait()
	return ctx.Err()
}

// cleanup restores sane defaults before exiting. Ignores canceled context!
func (a *computeBladeAgentImpl) cleanup(ctx context.Context) {
	log.FromContext(ctx).Info("Exiting, restoring safe settings")
	if err := a.blade.SetFanSpeed(100); err != nil {
		log.FromContext(ctx).Error("Failed to set fan speed to 100%", zap.Error(err))
	}
	if err := a.blade.SetLed(hal.LedEdge, hal.LedColor{}); err != nil {
		log.FromContext(ctx).Error("Failed to set edge LED to off", zap.Error(err))
	}
	if err := a.blade.SetLed(hal.LedTop, hal.LedColor{}); err != nil {
		log.FromContext(ctx).Error("Failed to set edge LED to off", zap.Error(err))
	}
}

func (a *computeBladeAgentImpl) handleEvent(ctx context.Context, event Event) error {
	log.FromContext(ctx).Info("Handling event", zap.String("event", event.String()))
	eventCounter.WithLabelValues(event.String()).Inc()

	// register event in state
	a.state.RegisterEvent(event)

	// Dispatch incoming events to the right handler(s)
	switch event {
	case CriticalEvent:
		// Handle critical event
		return a.handleCriticalActive(ctx)
	case CriticalResetEvent:
		// Handle critical event
		return a.handleCriticalReset(ctx)
	case IdentifyEvent:
		// Handle identify event
		return a.handleIdentifyActive(ctx)
	case IdentifyConfirmEvent:
		// Handle identify event
		return a.handleIdentifyConfirm(ctx)
	case EdgeButtonEvent:
		// Handle edge button press to toggle identify mode
		event := Event(IdentifyEvent)
		if a.state.IdentifyActive() {
			event = Event(IdentifyConfirmEvent)
		}
		select {
		case a.eventChan <- Event(event):
		default:
			log.FromContext(ctx).Warn("Edge button press event dropped due to backlog")
			droppedEventCounter.WithLabelValues(event.String()).Inc()
		}
	}

	return nil
}

func (a *computeBladeAgentImpl) handleIdentifyActive(ctx context.Context) error {
	log.FromContext(ctx).Info("Identify active")
	return a.edgeLedEngine.SetPattern(ledengine.NewBurstPattern(hal.LedColor{}, a.opts.IdentifyLedColor))
}

func (a *computeBladeAgentImpl) handleIdentifyConfirm(ctx context.Context) error {
	log.FromContext(ctx).Info("Identify confirmed/cleared")
	return a.edgeLedEngine.SetPattern(ledengine.NewStaticPattern(a.opts.IdleLedColor))
}

func (a *computeBladeAgentImpl) handleCriticalActive(ctx context.Context) error {
	log.FromContext(ctx).Warn("Blade in critical state, setting fan speed to 100% and turning on LEDs")

	// Set fan speed to 100%
	setFanspeedError := a.blade.SetFanSpeed(100)

	// Disable stealth mode (turn on LEDs)
	setStealthModeError := a.blade.SetStealthMode(false)

	// Set critical pattern for top LED
	setPatternTopLedErr := a.topLedEngine.SetPattern(
		ledengine.NewSlowBlinkPattern(hal.LedColor{}, a.opts.CriticalLedColor),
	)
	// Combine errors, but don't stop execution flow for now
	return errors.Join(setFanspeedError, setStealthModeError, setPatternTopLedErr)
}

func (a *computeBladeAgentImpl) handleCriticalReset(ctx context.Context) error {
	log.FromContext(ctx).Info("Critical state cleared, setting fan speed to default and restoring LEDs to default state")
	// Set fan speed to 100%
	if err := a.blade.SetFanSpeed(uint8(a.opts.DefaultFanSpeed)); err != nil {
		return err
	}

	// Reset stealth mode
	if err := a.blade.SetStealthMode(a.opts.StealthModeEnabled); err != nil {
		return err
	}

	// Set top LED off
	if err := a.topLedEngine.SetPattern(ledengine.NewStaticPattern(hal.LedColor{})); err != nil {
		return err
	}

	return nil
}

func (a *computeBladeAgentImpl) Close() error {
	return errors.Join(a.blade.Close())
}

// runTopLedEngine runs the top LED engine
func (a *computeBladeAgentImpl) runTopLedEngine(ctx context.Context) error {
	// FIXME the top LED is only used to indicate emergency situations
	err := a.topLedEngine.SetPattern(ledengine.NewStaticPattern(hal.LedColor{}))
	if err != nil {
		return err
	}
	return a.topLedEngine.Run(ctx)
}

// runEdgeLedEngine runs the edge LED engine
func (a *computeBladeAgentImpl) runEdgeLedEngine(ctx context.Context) error {
	err := a.edgeLedEngine.SetPattern(ledengine.NewStaticPattern(a.opts.IdleLedColor))
	if err != nil {
		return err
	}
	return a.edgeLedEngine.Run(ctx)
}

// EmitEvent dispatches an event to the event handler
func (a *computeBladeAgentImpl) EmitEvent(ctx context.Context, event Event) error {
	select {
	case a.eventChan <- event:
		return nil
	case <- ctx.Done():
		return ctx.Err()
	}
}

// SetFanSpeed sets the fan speed
func (a *computeBladeAgentImpl) SetFanSpeed(_ context.Context, speed uint8) error {
	if a.state.CriticalActive() {
		return errors.New("cannot set fan speed while the blade is in a critical state")
	}
	return a.blade.SetFanSpeed(speed)
}

// SetStealthMode enables/disables the stealth mode
func (a *computeBladeAgentImpl) SetStealthMode(_ context.Context, enabled bool) error {
	if a.state.CriticalActive() {
		return errors.New("cannot set stealth mode while the blade is in a critical state")
	}
	return a.blade.SetStealthMode(enabled)
}

// WaitForIdentifyConfirm waits for the identify confirm event
func (a *computeBladeAgentImpl) WaitForIdentifyConfirm(ctx context.Context) error {
	return a.state.WaitForIdentifyConfirm(ctx)
}
