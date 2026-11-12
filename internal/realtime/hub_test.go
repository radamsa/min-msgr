package realtime

import "testing"

func TestHubPublishesOnlyToMatchingChat(t *testing.T) {
	hub := NewHub()
	first := hub.Subscribe(1)
	defer first.Close()
	second := hub.Subscribe(2)
	defer second.Close()

	hub.Publish(1, Event{Type: "message.created", Payload: "hello"})

	select {
	case event := <-first.Events():
		if event.Type != "message.created" || event.Payload != "hello" {
			t.Errorf("event = %+v", event)
		}
	default:
		t.Fatal("first subscription received no event")
	}
	select {
	case event := <-second.Events():
		t.Fatalf("second subscription received %+v", event)
	default:
	}
}

func TestHubCloseStopsDelivery(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe(1)
	sub.Close()
	hub.Publish(1, Event{Type: "message.created"})

	if _, ok := <-sub.Events(); ok {
		t.Fatal("closed subscription still delivers events")
	}
}
