package main

import (
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v2"
	"github.com/pion/webrtc/v2/examples/internal/signal"
)

func main() {
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{}
	signal.Decode(signal.MustReadStdin(), &offer)

	// We make our own mediaEngine so we can place the sender's codecs in it. Since we are echoing their RTP packet
	// back to them we are actually codec agnostic - we can accept all their codecs. This also ensures that we use the
	// dynamic media type from the sender in our answer.
	mediaEngine := webrtc.MediaEngine{}

	// Add codecs to the mediaEngine. Note that even though we are only going to echo back the sender's video we also
	// add audio codecs. This is because createAnswer will create an audioTransceiver and associated SDP and we currently
	// cannot tell it not to. The audio SDP must match the sender's codecs too...
	err := mediaEngine.PopulateFromSDP(offer)
	if err != nil {
		panic(err)
	}

	videoCodecs := mediaEngine.GetCodecsByKind(webrtc.RTPCodecTypeVideo)
	if len(videoCodecs) == 0 {
		panic("Offer contained no video codecs")
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// Create Track that we send video back to browser on
	outputTrack1, err := peerConnection.NewTrack(videoCodecs[0].PayloadType, rand.Uint32(), "video1", "pion1")
	if err != nil {
		panic(err)
	}

	// Add this newly created track to the PeerConnection
	sender, err := peerConnection.AddTrack(outputTrack1)
	if err != nil {
		panic(err)
	}

	// Create a second track to put on video but don't add to connection
	outputTrack2, err := peerConnection.NewTrack(videoCodecs[0].PayloadType, rand.Uint32(), "video2", "pion2")
	if err != nil {
		panic(err)
	}

	// The AddTrack before will add a send/recv transceiver, but we need another
	// recv-only one to get the other track from the brwoser
	_, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RtpTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	if err != nil {
		panic(err)
	}

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Set a handler for when a new remote track starts, this handler copies inbound RTP packets,
	// replaces the SSRC and sends them back
	trackCount := 0
	peerConnection.OnTrack(func(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
		var outputTrack *webrtc.Track
		if trackCount == 0 {
			outputTrack = outputTrack1
		} else if trackCount == 1 {
			outputTrack = outputTrack2
		} else {
			panic("Got third track")
		}
		trackCount++
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		// This is a temporary fix until we implement incoming RTCP events, then we would push a PLI only when a viewer requests it
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for range ticker.C {
				errSend := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()}})
				if errSend != nil {
					fmt.Println(errSend)
				}
			}
		}()

		fmt.Printf("Track has started, of type %d: %s \n", track.PayloadType(), track.Codec().Name)
		for {
			// Read RTP packets being sent to Pion
			rtp, readErr := track.ReadRTP()
			if readErr != nil {
				panic(readErr)
			}

			// Replace the SSRC with the SSRC of the first outbound track.
			// The only change we are making replacing the SSRC, the RTP packets are unchanged otherwise
			rtp.SSRC = outputTrack1.SSRC()

			// Ignore closed pipe errors just in case we've switched tracks
			if writeErr := outputTrack.WriteRTP(rtp); writeErr != nil && writeErr != io.ErrClosedPipe {
				panic(writeErr)
			}
		}
	})
	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	connected := make(chan struct{}, 1)
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Output the answer in base64 so we can paste it in browser
	fmt.Println(signal.Encode(answer))

	// Rotate the track every so often
	fmt.Printf("Waiting for connection\n")
	<-connected
	// TODO: Also support ReplaceTrack(nil) to demonstrate turning track off
	curr := 1
	for {
		fmt.Printf("Waiting 10 seconds then changing...\n")
		time.Sleep(10 * time.Second)
		switch curr {
		case 0:
			fmt.Printf("Switching to track 1\n")
			if err := sender.ReplaceTrack(outputTrack1); err != nil {
				panic(err)
			}
		case 1:
			fmt.Printf("Switching to track 2\n")
			if err := sender.ReplaceTrack(outputTrack2); err != nil {
				panic(err)
			}
			// TODO: case 2: ReplaceTrack(null)
		}
		curr++
		if curr >= 2 {
			curr = 0
		}
	}
}
