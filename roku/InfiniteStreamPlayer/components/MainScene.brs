sub init()
    ' Initialize component references
    m.videoPlayer = m.top.findNode("videoPlayer")
    m.statusLabel = m.top.findNode("statusLabel")
    m.serverValue = m.top.findNode("serverValue")
    m.protocolValue = m.top.findNode("protocolValue")
    m.segmentValue = m.top.findNode("segmentValue")
    m.codecValue = m.top.findNode("codecValue")
    m.contentValue = m.top.findNode("contentValue")
    m.urlValue = m.top.findNode("urlValue")
    
    ' Initialize state
    m.serverEnvironments = [
        {label: "Dev (40000)", host: "100.111.190.54", contentPort: "40000", playbackPort: "40081"},
        {label: "Release (30000)", host: "infinitestreaming.jeoliver.com", contentPort: "30000", playbackPort: "30081"}
    ]
    m.currentServerIndex = 0
    
    m.protocols = ["HLS", "DASH"]
    m.currentProtocolIndex = 0
    
    m.segments = ["LL", "2s", "6s", "All"]
    m.currentSegmentIndex = 2  ' Default to 6s
    
    m.codecs = ["H264", "H265/HEVC", "AV1", "Auto"]
    m.currentCodecIndex = 0  ' Default to H264
    
    m.availableContent = []
    m.currentContentIndex = 0
    m.playerId = generatePlayerId()
    
    ' Set up key event observation
    m.top.setFocus(true)
    m.top.observeField("focusedChild", "onFocusedChildChange")
    
    ' Update display
    updateDisplay()
    
    ' Start content fetch
    fetchContentList()
    
    ' Observe video player state
    m.videoPlayer.observeField("state", "onVideoStateChange")
    m.videoPlayer.observeField("position", "onVideoPositionChange")
end sub

function generatePlayerId() as String
    ' Generate a simple player ID
    dt = CreateObject("roDateTime")
    return "roku_" + dt.asSeconds().toStr()
end function

sub onKeyEvent(key as String, press as Boolean) as Boolean
    if not press then return false
    
    handled = false
    
    if key = "left"
        ' Cycle through settings (backwards)
        cycleSettingsBackward()
        handled = true
    else if key = "right"
        ' Cycle through settings (forward)
        cycleSettingsForward()
        handled = true
    else if key = "up"
        ' Cycle server
        cycleServer()
        handled = true
    else if key = "down"
        ' Cycle content
        cycleContent()
        handled = true
    else if key = "OK"
        ' Play/Pause or reload
        togglePlayback()
        handled = true
    else if key = "replay" or key = "options"
        ' Restart playback
        restartPlayback()
        handled = true
    end if
    
    return handled
end sub

sub cycleServer()
    m.currentServerIndex = (m.currentServerIndex + 1) mod m.serverEnvironments.count()
    updateDisplay()
    fetchContentList()
end sub

sub cycleSettingsForward()
    ' Cycle through: Protocol -> Segment -> Codec
    ' For simplicity, cycle protocol
    m.currentProtocolIndex = (m.currentProtocolIndex + 1) mod m.protocols.count()
    updateDisplay()
    applySelection()
end sub

sub cycleSettingsBackward()
    m.currentProtocolIndex = (m.currentProtocolIndex - 1 + m.protocols.count()) mod m.protocols.count()
    updateDisplay()
    applySelection()
end sub

sub cycleContent()
    if m.availableContent.count() > 0
        m.currentContentIndex = (m.currentContentIndex + 1) mod m.availableContent.count()
        updateDisplay()
        applySelection()
    end if
end sub

sub togglePlayback()
    if m.videoPlayer.state = "playing"
        m.videoPlayer.control = "pause"
        m.statusLabel.text = "Paused"
    else if m.videoPlayer.state = "paused"
        m.videoPlayer.control = "resume"
        m.statusLabel.text = "Playing"
    else
        ' Start playback
        applySelection()
    end if
end sub

sub restartPlayback()
    m.statusLabel.text = "Restarting playback..."
    applySelection()
end sub

sub updateDisplay()
    ' Update server
    env = m.serverEnvironments[m.currentServerIndex]
    m.serverValue.text = env.label
    
    ' Update protocol
    m.protocolValue.text = m.protocols[m.currentProtocolIndex]
    
    ' Update segment
    m.segmentValue.text = m.segments[m.currentSegmentIndex]
    
    ' Update codec
    m.codecValue.text = m.codecs[m.currentCodecIndex]
    
    ' Update content
    if m.availableContent.count() > 0
        content = m.availableContent[m.currentContentIndex]
        m.contentValue.text = content.name
    else
        m.contentValue.text = "No content available"
    end if
end sub

sub fetchContentList()
    m.statusLabel.text = "Fetching content list..."
    
    env = m.serverEnvironments[m.currentServerIndex]
    url = "http://" + env.host + ":" + env.contentPort + "/api/content"
    
    request = CreateObject("roUrlTransfer")
    request.SetUrl(url)
    request.SetCertificatesFile("common:/certs/ca-bundle.crt")
    request.InitClientCertificates()
    request.EnablePeerVerification(false)
    request.EnableHostVerification(false)
    
    port = CreateObject("roMessagePort")
    request.SetPort(port)
    
    if request.AsyncGetToString()
        msg = wait(10000, port)  ' 10 second timeout
        if type(msg) = "roUrlEvent"
            if msg.GetResponseCode() = 200
                responseString = msg.GetString()
                parseContentList(responseString)
            else
                m.statusLabel.text = "Failed to fetch content: HTTP " + msg.GetResponseCode().toStr()
            end if
        else
            m.statusLabel.text = "Request timeout"
        end if
    else
        m.statusLabel.text = "Failed to start request"
    end if
end sub

sub parseContentList(jsonString as String)
    json = ParseJson(jsonString)
    if json <> invalid and type(json) = "roArray"
        m.availableContent = []
        for each item in json
            if item.has_hls = true or item.has_dash = true
                ' Filter to H264 content only (matching iOS behavior)
                contentName = item.name
                if not (LCase(contentName).Instr("hevc") >= 0 or LCase(contentName).Instr("h265") >= 0 or LCase(contentName).Instr("av1") >= 0)
                    m.availableContent.push(item)
                end if
            end if
        end for
        
        if m.availableContent.count() > 0
            m.currentContentIndex = 0
            updateDisplay()
            m.statusLabel.text = "Loaded " + m.availableContent.count().toStr() + " items"
            
            ' Auto-play default content
            applySelection()
        else
            m.statusLabel.text = "No compatible content found"
        end if
    else
        m.statusLabel.text = "Failed to parse content list"
    end if
end sub

sub applySelection()
    if m.availableContent.count() = 0
        m.statusLabel.text = "No content to play"
        return
    end if
    
    content = m.availableContent[m.currentContentIndex]
    protocol = m.protocols[m.currentProtocolIndex]
    segment = m.segments[m.currentSegmentIndex]
    
    ' Check if content supports selected protocol
    if protocol = "HLS" and not content.has_hls
        m.statusLabel.text = "Content does not support HLS"
        return
    end if
    
    if protocol = "DASH" and not content.has_dash
        m.statusLabel.text = "Content does not support DASH"
        return
    end if
    
    ' Build stream URL
    env = m.serverEnvironments[m.currentServerIndex]
    streamUrl = buildStreamURL(env, content.name, protocol, segment)
    
    ' Update URL display
    m.urlValue.text = streamUrl
    
    ' Play the stream
    playStream(streamUrl, content.name)
end sub

function buildStreamURL(env as Object, contentName as String, protocol as String, segment as String) as String
    baseUrl = "http://" + env.host + ":" + env.contentPort
    path = "/go-live/" + contentName + "/"
    
    if protocol = "DASH"
        if segment = "2s"
            path = path + "manifest_2s.mpd"
        else if segment = "6s"
            path = path + "manifest_6s.mpd"
        else
            path = path + "manifest.mpd"  ' LL or All
        end if
    else  ' HLS
        if segment = "2s"
            path = path + "master_2s.m3u8"
        else if segment = "6s"
            path = path + "master_6s.m3u8"
        else
            path = path + "master.m3u8"  ' LL or All
        end if
    end if
    
    ' Add player_id query parameter
    streamUrl = baseUrl + path + "?player_id=" + m.playerId
    
    return streamUrl
end function

sub playStream(url as String, contentName as String)
    m.statusLabel.text = "Loading: " + contentName
    
    ' Create video content
    videoContent = CreateObject("roSGNode", "ContentNode")
    videoContent.url = url
    videoContent.title = contentName
    videoContent.streamFormat = getStreamFormat()
    
    ' Set the content and play
    m.videoPlayer.content = videoContent
    m.videoPlayer.control = "play"
    m.videoPlayer.SetFocus(true)
end sub

function getStreamFormat() as String
    protocol = m.protocols[m.currentProtocolIndex]
    if protocol = "DASH"
        return "dash"
    else
        return "hls"
    end if
end function

sub onVideoStateChange()
    state = m.videoPlayer.state
    
    if state = "playing"
        m.statusLabel.text = "Playing"
    else if state = "paused"
        m.statusLabel.text = "Paused"
    else if state = "buffering"
        m.statusLabel.text = "Buffering..."
    else if state = "finished"
        m.statusLabel.text = "Finished"
    else if state = "error"
        m.statusLabel.text = "Playback error"
    else if state = "stopped"
        m.statusLabel.text = "Stopped"
    end if
end sub

sub onVideoPositionChange()
    ' Could display current position/duration here if desired
end sub

sub onFocusedChildChange()
    ' Handle focus changes if needed
end sub
