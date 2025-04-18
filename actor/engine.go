package actor

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"time"
)

// Remoter is an interface that abstract a remote that is tied to an engine.
type Remoter interface {
	Address() string
	Send(*PID, any, *PID)
	Start(*Engine) error
	Stop() *sync.WaitGroup
}

// Producer is any function that can return a Receiver
type Producer func() Receiver

// Receiver is an interface that can receive and process messages.
type Receiver interface {
	Receive(*Context)
}

// Engine represents the actor engine.
type Engine struct {
	Registry    *Registry
	address     string
	remote      Remoter
	eventStream *PID
}

// EngineConfig holds the configuration of the engine.
type EngineConfig struct {
	remote Remoter
}

// NewEngineConfig returns a new default EngineConfig.
func NewEngineConfig() EngineConfig {
	return EngineConfig{}
}

// WithRemote sets the remote which will configure the engine so its capable
// to send and receive messages over the network.
func (config EngineConfig) WithRemote(remote Remoter) EngineConfig {
	config.remote = remote
	return config
}

// NewEngine returns a new actor Engine given an EngineConfig.
func NewEngine(config EngineConfig) (*Engine, error) {
	e := &Engine{}
	e.Registry = newRegistry(e) // need to init the registry in case we want a custom deadletter
	e.address = LocalLookupAddr
	if config.remote != nil {
		e.remote = config.remote
		e.address = config.remote.Address()
		err := config.remote.Start(e)
		if err != nil {
			return nil, fmt.Errorf("failed to start remote: %w", err)
		}
	}
	e.eventStream = e.Spawn(newEventStream(), "eventstream")
	return e, nil
}

// Spawn spawns a process that will producer by the given Producer and
// can be configured with the given opts.
func (e *Engine) Spawn(p Producer, kind string, opts ...OptFunc) *PID {
	options := DefaultOpts(p)
	options.Kind = kind
	for _, opt := range opts {
		opt(&options)
	}
	// Check if we got an ID, generate otherwise
	if len(options.ID) == 0 {
		id := strconv.Itoa(rand.Intn(math.MaxInt))
		options.ID = id
	}
	proc := newProcess(e, options)
	return e.SpawnProc(proc)
}

// SpawnFunc spawns the given function as a stateless receiver/actor.
func (e *Engine) SpawnFunc(f func(*Context), kind string, opts ...OptFunc) *PID {
	return e.Spawn(newFuncReceiver(f), kind, opts...)
}

// SpawnProc spawns the give Processer. This function is useful when working
// with custom created Processes. Take a look at the streamWriter as an example.
func (e *Engine) SpawnProc(p Processer) *PID {
	e.Registry.add(p)
	return p.PID()
}

// Address returns the address of the actor engine. When there is
// no remote configured, the "local" address will be used, otherwise
// the listen address of the remote.
func (e *Engine) Address() string {
	return e.address
}

// Request sends the given message to the given PID as a "Request", returning
// a response that will resolve in the future. Calling Response.Result() will
// block until the deadline is exceeded or the response is being resolved.
func (e *Engine) Request(pid *PID, msg any, timeout time.Duration) *Response {
	resp := NewResponse(e, timeout)
	e.Registry.add(resp)

	e.SendWithSender(pid, msg, resp.PID())

	return resp
}

// SendWithSender will send the given message to the given PID with the
// given sender. Receivers receiving this message can check the sender
// by calling Context.Sender().
func (e *Engine) SendWithSender(pid *PID, msg any, sender *PID) {
	e.send(pid, msg, sender)
}

// Send sends the given message to the given PID. If the message cannot be
// delivered due to the fact that the given process is not registered.
// The message will be sent to the DeadLetter process instead.
func (e *Engine) Send(pid *PID, msg any) {
	e.send(pid, msg, nil)
}

// BroadcastEvent will broadcast the given message over the eventstream, notifying all
// actors that are subscribed.
func (e *Engine) BroadcastEvent(msg any) {
	if e.eventStream != nil {
		e.send(e.eventStream, msg, nil)
	}
}

func (e *Engine) send(pid *PID, msg any, sender *PID) {
	// TODO: We might want to log something here. Not yet decided
	// what could make sense. Send to dead letter or as event?
	// Dead letter would make sense cause the destination is not
	// reachable.
	if pid == nil {
		return
	}
	if e.isLocalMessage(pid) {
		e.SendLocal(pid, msg, sender)
		return
	}
	if e.remote == nil {
		e.BroadcastEvent(EngineRemoteMissingEvent{Target: pid, Sender: sender, Message: msg})
		return
	}
	e.remote.Send(pid, msg, sender)
}

// SendRepeater is a struct that can be used to send a repeating message to a given PID.
// If you need to have an actor wake up periodically, you can use a SendRepeater.
// It is started by the SendRepeat method and stopped by it's Stop() method.
type SendRepeater struct {
	engine   *Engine
	self     *PID
	target   *PID
	msg      any
	interval time.Duration
	cancelch chan struct{}
}

func (sr SendRepeater) start() {
	ticker := time.NewTicker(sr.interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				sr.engine.SendWithSender(sr.target, sr.msg, sr.self)
			case <-sr.cancelch:
				ticker.Stop()
				return
			}
		}
	}()
}

// Stop will stop the repeating message.
func (sr SendRepeater) Stop() {
	close(sr.cancelch)
}

// SendRepeat will send the given message to the given PID each given interval.
// It will return a SendRepeater struct that can stop the repeating message by calling Stop().
func (e *Engine) SendRepeat(pid *PID, msg any, interval time.Duration) SendRepeater {
	clonedPID := *pid.CloneVT()
	sr := SendRepeater{
		engine:   e,
		self:     nil,
		target:   &clonedPID,
		interval: interval,
		msg:      msg,
		cancelch: make(chan struct{}, 1),
	}
	sr.start()
	return sr
}

// Stop will send a non-graceful poisonPill message to the process that is associated with the given PID.
// The process will shut down immediately. A context is being returned that can be used to block / wait
// until the process is stopped.
func (e *Engine) Stop(pid *PID) context.Context {
	return e.sendPoisonPill(context.Background(), false, pid)
}

// Poison will send a graceful poisonPill message to the process that is associated with the given PID.
// The process will shut down gracefully once it has processed all the messages in the inbox.
// A context is returned that can be used to block / wait until the process is stopped.
func (e *Engine) Poison(pid *PID) context.Context {
	return e.sendPoisonPill(context.Background(), true, pid)
}

// PoisonCtx behaves the exact same as Poison, the only difference is that it accepts
// a context as the first argument. The context can be used for custom timeouts and manual
// cancelation.
func (e *Engine) PoisonCtx(ctx context.Context, pid *PID) context.Context {
	return e.sendPoisonPill(ctx, true, pid)
}

func (e *Engine) sendPoisonPill(ctx context.Context, graceful bool, pid *PID) context.Context {
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	pill := poisonPill{
		cancel:   cancel,
		graceful: graceful,
	}
	// deadletter - if we didn't find a process, we will broadcast a DeadletterEvent
	if e.Registry.get(pid) == nil {
		e.BroadcastEvent(DeadLetterEvent{
			Target:  pid,
			Message: pill,
			Sender:  nil,
		})
		cancel()
		return ctx
	}
	e.SendLocal(pid, pill, nil)
	return ctx
}

// SendLocal will send the given message to the given PID. If the recipient is not found in the
// registry, the message will be sent to the DeadLetter process instead. If there is no deadletter
// process registered, the function will panic.
func (e *Engine) SendLocal(pid *PID, msg any, sender *PID) {
	proc := e.Registry.get(pid)
	if proc == nil {
		// broadcast a deadLetter message
		e.BroadcastEvent(DeadLetterEvent{
			Target:  pid,
			Message: msg,
			Sender:  sender,
		})
		return
	}
	proc.Send(pid, msg, sender)
}

// Subscribe will subscribe the given PID to the event stream.
func (e *Engine) Subscribe(pid *PID) {
	e.Send(e.eventStream, eventSub{pid: pid})
}

// Unsubscribe will un subscribe the given PID from the event stream.
func (e *Engine) Unsubscribe(pid *PID) {
	e.Send(e.eventStream, eventUnsub{pid: pid})
}

func (e *Engine) isLocalMessage(pid *PID) bool {
	if pid == nil {
		return false
	}
	return e.address == pid.Address
}

type funcReceiver struct {
	f func(*Context)
}

func newFuncReceiver(f func(*Context)) Producer {
	return func() Receiver {
		return &funcReceiver{
			f: f,
		}
	}
}

func (r *funcReceiver) Receive(c *Context) {
	r.f(c)
}
