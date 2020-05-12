# simulcast-hack
simulcast-hack demonstrates how accept multiple incoming simulcasted streams and how to swap them on a single outgoing
track.

## Instructions
### Download simulcast-hack
```
go get github.com/pion/webrtc/v2/examples/simulcast-hack
```

### Open simulcast-hack example page
Open the `index.html` file in a browser, you should see a 'Capture Camera' button. Once clicked and camera chosen, you
should see two text-areas and a 'Start Session' button.

### Run simulcast-hack, with your browsers SessionDescription as stdin
On the page the top textarea is your browser, copy that and:
#### Linux/macOS
Run `echo $BROWSER_SDP | simulcast-hack`
#### Windows
1. Paste the SessionDescription into a file.
1. Run `simulcast-hack < my_file`

### Input simulcast-hack's SessionDescription into your browser
Copy the text that `simulcast-hack` just emitted and copy into second text area

### Hit 'Start Session' on the page, enjoy your video!
Your browser should send streams to Pion, and then you can choose to change the quality of the stream going to the page.

Congrats, you have used Pion WebRTC! Now start building something cool

**NOTE: Only tested with Chrome so far**
