// Package notify sends OS desktop notifications for incoming WhatsApp
// messages. It's a thin wrapper around beeep so the rest of the codebase
// doesn't depend on a third-party notification library directly.
package notify

import "github.com/gen2brain/beeep"

type request struct {
	title   string
	body    string
	onError func(error)
}

// dispatcher keeps desktop notification IPC away from whatsmeow's event
// handler. Some notification daemons can leave the D-Bus call blocked for a
// long time; doing that work inline prevents whatsmeow from processing the
// responses to its own requests (including the device-list usync used while
// sending messages).
type dispatcher struct {
	queue chan request
	send  func(string, string, any) error
}

func newDispatcher(send func(string, string, any) error) *dispatcher {
	d := &dispatcher{
		queue: make(chan request, 1),
		send:  send,
	}
	go d.run()
	return d
}

func (d *dispatcher) run() {
	for req := range d.queue {
		if err := d.send(req.title, req.body, ""); err != nil && req.onError != nil {
			req.onError(err)
		}
	}
}

func (d *dispatcher) enqueue(req request) bool {
	select {
	case d.queue <- req:
		return true
	default:
		return false
	}
}

var desktop = newDispatcher(beeep.Notify)

// Send shows a desktop notification with the given title and body.
// Desktop notification support varies a lot by OS/environment (e.g. a
// minimal Linux setup without notify-send installed) — callers should
// log a failure here, not treat it as fatal, since a missed popup
// shouldn't take down message receiving.
func Send(title, body string) error {
	return beeep.Notify(title, body, "")
}

// SendAsync queues a desktop notification without waiting for D-Bus or an
// external notification command. It returns false when a previous notification
// is stuck and the one-entry queue is already occupied; callers should treat a
// dropped desktop notification as non-fatal.
func SendAsync(title, body string, onError func(error)) bool {
	return desktop.enqueue(request{title: title, body: body, onError: onError})
}
