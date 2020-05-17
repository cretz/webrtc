package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v2"
	"github.com/pion/webrtc/v2"
	"github.com/pion/webrtc/v2/examples/internal/signal"
)

func main() {
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.

	// Wait for the offer to be pasted and parse it
	offer := webrtc.SessionDescription{}
	signal.Decode(signal.MustReadStdin(), &offer)
	var offerDesc sdp.SessionDescription
	if err := offerDesc.Unmarshal([]byte(offer.SDP)); err != nil {
		panic(err)
	}

	// Find the ID of the rid extension for later use
	var offerVideo *sdp.MediaDescription
	for _, md := range offerDesc.MediaDescriptions {
		if md.MediaName.Media == "video" {
			if offerVideo != nil {
				panic("Found multiple video descriptions in offer")
			}
			offerVideo = md
		}
	}
	if offerVideo == nil {
		panic("Found no video descriptions in offer")
	}
	var ridExt *sdp.ExtMap
	for _, attr := range offerVideo.Attributes {
		if attr.Key == "extmap" {
			var ext sdp.ExtMap
			if ext.Unmarshal(*attr.String()) == nil && ext.URI.String() == "urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id" {
				ridExt = &ext
				break
			}
		}
	}
	if ridExt == nil {
		panic("Unable to find RTP stream ID extension in offer")
	}

	// Just used a fixed VP8 codec
	mediaEngine := webrtc.MediaEngine{}
	mediaEngine.RegisterCodec(webrtc.NewRTPVP8Codec(webrtc.DefaultPayloadTypeVP8, 90000))

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
		// New config to lazily accept SSRCs as video
		AcceptUndeclaredSSRCAsVideo: true,
	}
	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// Create Track that we send video back to browser on, and a transceiver for
	// the other
	outputTrack, err := peerConnection.NewTrack(webrtc.DefaultPayloadTypeVP8, rand.Uint32(), "video", "pion")
	if err != nil {
		panic(err)
	}

	// Add this newly created track to the PeerConnection
	if _, err = peerConnection.AddTrack(outputTrack); err != nil {
		panic(err)
	}

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Total max bitrate to tell browser we can upload. Usually this would be
	// derived based on sender/receiver reports, but for this is example a fixed
	// value is fine (5mbps here).
	const maxTotalBitrate = 5000000

	// This is the RTP stream ID to send (we don't care if updates are atomic)
	var currRTPStreamID byte
	currRTPStreamID = 'a'
	// The channel of packets with a bit of buffer
	packets := make(chan *rtp.Packet, 60)

	// Set a handler for when a new remote track starts
	peerConnection.OnTrack(func(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
		// Right at the beginning and then every so often, send the max bitrate to
		// the browser. If this is not done, the browser assumes a max (300kbps) and
		// doesn't send some streams.
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			rtcpPackets := []rtcp.Packet{
				&rtcp.ReceiverEstimatedMaximumBitrate{
					SenderSSRC: outputTrack.SSRC(),
					Bitrate:    maxTotalBitrate,
					SSRCs:      []uint32{track.SSRC()},
				},
				// Also send a PLI every so often to get picture refresh
				&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()},
			}
			if rtcpSendErr := peerConnection.WriteRTCP(rtcpPackets); rtcpSendErr != nil {
				fmt.Println(rtcpSendErr)
			}
			for range ticker.C {
				if rtcpSendErr := peerConnection.WriteRTCP(rtcpPackets); rtcpSendErr != nil {
					fmt.Println(rtcpSendErr)
				}
			}
		}()

		// The last timestamp so that we can change the packet to only be the delta
		var lastTimestamp uint32

		// Whether this track is the one currently sending to the channel (on change
		// of this we send a PLI to have the entire picture updated)
		var isCurrTrack bool
		for {
			// Read the packet
			rtp, err := track.ReadRTP()
			if err != nil {
				panic(err)
			}

			// Change the timestamp to only be the delta
			oldTimestamp := rtp.Timestamp
			if lastTimestamp == 0 {
				rtp.Timestamp = 0
			} else {
				rtp.Timestamp -= lastTimestamp
			}
			lastTimestamp = oldTimestamp

			// Check the RTP stream ID
			extPayload := rtp.GetExtension(uint8(ridExt.Value))
			if len(extPayload) == 0 {
				panic("Expected RTP Stream ID in extension, did not get")
			} else if extPayload[0] != currRTPStreamID {
				// Skip if not the current stream ID
				isCurrTrack = false
				continue
			}
			// If just switched to this track, send PLI to get picture refresh
			if !isCurrTrack {
				isCurrTrack = true
				if writeErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()}}); writeErr != nil {
					fmt.Println(writeErr)
				}
			}
			// Send to channel
			packets <- rtp
		}
	})

	// Set the handler for the data channel
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnMessage(func(m webrtc.DataChannelMessage) {
			v := struct {
				RID string `json:"rid"`
			}{}
			if err := json.Unmarshal(m.Data, &v); err != nil {
				panic(err)
			}
			fmt.Printf("Changing stream to %q\n", v.RID)
			currRTPStreamID = v.RID[0]
		})
	})

	// Set the handler for ICE connection state and update chan if connected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
	})

	// Asynchronously take all packets in the channel and write them out to our
	// track
	go func() {
		var currTimestamp uint32
		// TODO: Note, best not to start at 0, but a random seq num
		for i := uint16(0); ; i++ {
			packet := <-packets
			// Timestamp on the packet is really a diff, so add it to current
			currTimestamp += packet.Timestamp
			packet.Timestamp = currTimestamp
			// Set the output SSRC
			packet.SSRC = outputTrack.SSRC()
			// Keep an increasing sequence number
			packet.SequenceNumber = i
			// Clear out extension stuff
			packet.Extension, packet.ExtensionProfile, packet.Extensions = false, 0, nil
			// Write out the packet, ignoring closed pipe if nobody is listening
			if err := outputTrack.WriteRTP(packet); err != nil && err != io.ErrClosedPipe {
				panic(err)
			}
		}
	}()

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

	// Now, after we have set the answer, change it to include simulcast stuff
	// before sending it back
	var answerDesc sdp.SessionDescription
	if err := answerDesc.Unmarshal([]byte(answer.SDP)); err != nil {
		panic(err)
	}
	// Just copy over every existing attribute, but when we get to the rtpmap one
	// add some extra stuff
	for _, md := range answerDesc.MediaDescriptions {
		if md.MediaName.Media == "video" {
			var attrs []sdp.Attribute
			for _, attr := range md.Attributes {
				attrs = append(attrs, attr)
				if attr.Key == "rtpmap" {
					// Add the rid ext
					attrs = append(attrs, ridExt.Clone())
					// Take all of the rid and simulcast attributes from the offer and add
					// them to the answer, but as recv instead of send
					for _, offerAttr := range offerVideo.Attributes {
						if offerAttr.Key == "rid" || offerAttr.Key == "simulcast" {
							attrs = append(attrs, sdp.NewAttribute(offerAttr.Key, strings.Replace(offerAttr.Value, "send", "recv", -1)))
						}
					}
				}
			}
			md.Attributes = attrs
		}
	}

	b, err := answerDesc.Marshal()
	if err != nil {
		panic(err)
	}
	answer.SDP = string(b)

	// Output the answer in base64 so we can paste it in browser
	fmt.Printf("Paste below base64 in browser:\n%v\n", signal.Encode(answer))

	select {}
}
