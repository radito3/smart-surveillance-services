# smart-surveillance-services
Backend services used for the Smart Surveillance System

### Transcoder
Makes a stream available on the following endpoints:

 - RTMP: rtmp://`<external-ip>`:1935/live/stream
 - HLS: http://`<external-ip>`:8080/hls/master.m3u8
 - DASH: http://`<external-ip>`:8080/dash/dash_640p.mpd
 - RTSP: rtsp://`<external-ip>`:8554/live.sdp
 - WebRTC: Managed by a separate signaling server like Janus Gateway or Pion WebRTC.
