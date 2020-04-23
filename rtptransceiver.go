// +build !js

package webrtc

import (
	"fmt"
	"sync/atomic"

	"github.com/pion/webrtc/v2/pkg/rtcerr"
)

// RTPTransceiver represents a combination of an RTPSender and an RTPReceiver that share a common mid.
type RTPTransceiver struct {
	sender    atomic.Value // *RTPSender
	receiver  atomic.Value // *RTPReceiver
	direction atomic.Value // RTPTransceiverDirection

	stopped bool
	kind    RTPCodecType
}

// Sender returns the RTPTransceiver's RTPSender if it has one
func (t *RTPTransceiver) Sender() *RTPSender {
	if v := t.sender.Load(); v != nil {
		return v.(*RTPSender)
	}

	return nil
}

func (t *RTPTransceiver) setSender(s *RTPSender) {
	// Set on sender
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.transceiver = t
	}
	// Remove from current sender if there
	if currSender := t.Sender(); currSender != nil {
		// Lock for rest of function for atomic swap
		currSender.mu.Lock()
		defer currSender.mu.Unlock()
		currSender.transceiver = nil
	}
	t.sender.Store(s)
}

// Receiver returns the RTPTransceiver's RTPReceiver if it has one
func (t *RTPTransceiver) Receiver() *RTPReceiver {
	if v := t.receiver.Load(); v != nil {
		return v.(*RTPReceiver)
	}

	return nil
}

// Direction returns the RTPTransceiver's current direction
func (t *RTPTransceiver) Direction() RTPTransceiverDirection {
	return t.direction.Load().(RTPTransceiverDirection)
}

// Stop irreversibly stops the RTPTransceiver
func (t *RTPTransceiver) Stop() error {
	if t.Sender() != nil {
		if err := t.Sender().Stop(); err != nil {
			return err
		}
	}
	if t.Receiver() != nil {
		if err := t.Receiver().Stop(); err != nil {
			return err
		}
	}

	t.setDirection(RTPTransceiverDirectionInactive)
	return nil
}

func (t *RTPTransceiver) setReceiver(r *RTPReceiver) {
	t.receiver.Store(r)
}

func (t *RTPTransceiver) setDirection(d RTPTransceiverDirection) {
	t.direction.Store(d)
}

func (t *RTPTransceiver) setSendingTrack(track *Track) error {
	t.Sender().track = track
	if track == nil {
		t.setSender(nil)
	}

	switch {
	case track != nil && t.Direction() == RTPTransceiverDirectionRecvonly:
		t.setDirection(RTPTransceiverDirectionSendrecv)
	case track != nil && t.Direction() == RTPTransceiverDirectionInactive:
		t.setDirection(RTPTransceiverDirectionSendonly)
	case track == nil && t.Direction() == RTPTransceiverDirectionSendrecv:
		t.setDirection(RTPTransceiverDirectionRecvonly)
	case track == nil && t.Direction() == RTPTransceiverDirectionSendonly:
		t.setDirection(RTPTransceiverDirectionInactive)
	default:
		return fmt.Errorf("invalid state change in RTPTransceiver.setSending")
	}
	return nil
}

// Expected to be called from RTPSender (sans lock). Steps inspired by
// https://w3c.github.io/webrtc-pc/#dom-rtcrtpsender-replacetrack.
func (t *RTPTransceiver) replaceTrack(track *Track) error {
	if track != nil && track.Kind() != t.kind {
		return &rtcerr.TypeError{Err: fmt.Errorf("track is kind %v, transceiver kind is %v", track.Kind(), t.kind)}
	} else if t.stopped {
		return &rtcerr.InvalidStateError{Err: fmt.Errorf("transceiver is stopped")}
	}
	// Should always be non-nil
	sender := t.Sender()
	// If it's not sending, just set the track
	if dir := t.Direction(); dir != RTPTransceiverDirectionSendrecv && dir != RTPTransceiverDirectionSendonly {
		return t.setSendingTrack(track)
	}
	// Nil track means stop the sender
	if track == nil {
		if err := sender.Stop(); err != nil {
			return err
		}
		return t.setSendingTrack(nil)
	}
	// Lock the sender and track from here on out
	sender.mu.Lock()
	defer sender.mu.Unlock()
	track.mu.Lock()
	defer track.mu.Unlock()
	if track.receiver != nil {
		return fmt.Errorf("cannot replace with remote track")
	}
	// Unset current track
	if sender.track != nil {
		// Lock existing track from here on out
		sender.track.mu.Lock()
		defer sender.track.mu.Unlock()
		// Confirm existing track doesn't require renegotiation. For now, we'll
		// just check the mime type.
		if sender.track.codec.MimeType != track.codec.MimeType {
			return &rtcerr.InvalidModificationError{
				Err: fmt.Errorf("replacement would require renegotiation, current track type %v, new track type is %v",
					sender.track.codec.MimeType, track.codec.MimeType),
			}
		}
		// Remove sender from active senders
		filtered := make([]*RTPSender, 0, len(sender.track.activeSenders)-1)
		for _, s := range sender.track.activeSenders {
			if s != sender {
				filtered = append(filtered, s)
			} else {
				sender.track.totalSenderCount--
			}
		}
		sender.track.activeSenders = filtered
	}
	// Set the new track
	sender.track = track
	track.activeSenders = append(track.activeSenders, sender)
	track.totalSenderCount++
	return nil
}

// Given a direction+type pluck a transceiver from the passed list
// if no entry satisfies the requested type+direction return a inactive Transceiver
func satisfyTypeAndDirection(remoteKind RTPCodecType, remoteDirection RTPTransceiverDirection, localTransceivers []*RTPTransceiver) (*RTPTransceiver, []*RTPTransceiver) {
	// Get direction order from most preferred to least
	getPreferredDirections := func() []RTPTransceiverDirection {
		switch remoteDirection {
		case RTPTransceiverDirectionSendrecv:
			return []RTPTransceiverDirection{RTPTransceiverDirectionRecvonly, RTPTransceiverDirectionSendrecv}
		case RTPTransceiverDirectionSendonly:
			return []RTPTransceiverDirection{RTPTransceiverDirectionRecvonly, RTPTransceiverDirectionSendrecv}
		case RTPTransceiverDirectionRecvonly:
			return []RTPTransceiverDirection{RTPTransceiverDirectionSendonly, RTPTransceiverDirectionSendrecv}
		}
		return []RTPTransceiverDirection{}
	}

	for _, possibleDirection := range getPreferredDirections() {
		for i := range localTransceivers {
			t := localTransceivers[i]
			if t.kind != remoteKind || possibleDirection != t.Direction() {
				continue
			}

			return t, append(localTransceivers[:i], localTransceivers[i+1:]...)
		}
	}

	d := atomic.Value{}
	d.Store(RTPTransceiverDirectionInactive)

	return &RTPTransceiver{
		kind:      remoteKind,
		direction: d,
	}, localTransceivers
}
