sub init()
    m.contentFetchTimeoutMs = 10000
    m.defaultServerIndex = 1
    m.defaultProtocolIndex = 0  ' HLS
    m.defaultSegmentIndex = 2   ' 6s
    m.defaultCodecIndex = 0     ' H264

    ' Initialize component references
    m.videoPlayer = m.top.findNode("videoPlayer")
    m.statusLabel = m.top.findNode("statusLabel")
    m.serverValue = m.top.findNode("serverValue")
    m.protocolValue = m.top.findNode("protocolValue")
    m.segmentValue = m.top.findNode("segmentValue")
    m.codecValue = m.top.findNode("codecValue")
    m.contentValue = m.top.findNode("contentValue")
    m.urlValue = m.top.findNode("urlValue")
    m.contentTask = m.top.findNode("contentFetchTask")
    
    ' Initialize state
    ' Note: These server addresses match the iOS app defaults
    m.serverEnvironments = [
        {label: "Dev (40000)", host: "100.111.190.54", contentPort: "40000", playbackPort: "40081"},
        {label: "Release (30000)", host: "infinitestreaming.jeoliver.com", contentPort: "30000", playbackPort: "30081"}
    ]
    m.currentServerIndex = m.defaultServerIndex
    
    m.protocols = ["HLS", "DASH"]
    m.currentProtocolIndex = m.defaultProtocolIndex
    
    m.segments = ["LL", "2s", "6s", "All"]
    m.currentSegmentIndex = m.defaultSegmentIndex
    
    m.codecs = ["H264", "H265/HEVC", "AV1", "Auto"]
    m.currentCodecIndex = m.defaultCodecIndex
    
    m.availableContent = []
    m.currentContentIndex = 0
    m.playerId = generatePlayerId()
    m.forceContentName = "INSANE_FPV_SHOTS_Hydrofoil_Windsurfing_p200_hevc"
    
    ' Set up key event observation
    m.top.setFocus(true)
    m.top.observeField("focusedChild", "onFocusedChildChange")

    m.contentTask.observeField("response", "onContentFetchResponse")
    m.contentTask.observeField("error", "onContentFetchError")
    
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

function onKeyEvent(key as String, press as Boolean) as Boolean
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
end function

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
    if m.forceContentName <> invalid and m.forceContentName <> ""
        m.contentValue.text = m.forceContentName
    else if m.availableContent.count() > 0
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
    
    m.contentTask.url = url
    m.contentTask.control = "RUN"
end sub

sub onContentFetchResponse()
    responseString = m.contentTask.response
    statusCode = m.contentTask.statusCode

    if responseString <> invalid and responseString <> ""
        parseContentList(responseString)
    else
        if statusCode > 0
            m.statusLabel.text = "Failed to fetch content: HTTP " + statusCode.toStr()
        else
            m.statusLabel.text = "Failed to fetch content"
        end if
    end if
end sub

sub onContentFetchError()
    errorMessage = m.contentTask.error
    if errorMessage <> invalid and errorMessage <> ""
        m.statusLabel.text = "Failed to fetch content: " + errorMessage
    else
        m.statusLabel.text = "Failed to fetch content"
    end if
end sub

function isH264Content(contentName as String) as Boolean
    ' Check if content is H264 by excluding HEVC/H265/AV1 (matching iOS app behavior)
    nameLower = LCase(contentName)
    return not (nameLower.Instr("hevc") >= 0 or nameLower.Instr("h265") >= 0 or nameLower.Instr("av1") >= 0)
end function

sub parseContentList(jsonString as String)
    json = ParseJson(jsonString)
    if json <> invalid and type(json) = "roArray"
        m.availableContent = []
        for each item in json
            if item.has_hls = true or item.has_dash = true
                ' Filter to H264 content only (matching iOS behavior)
                if isH264Content(item.name)
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
    if m.forceContentName <> invalid and m.forceContentName <> ""
        content = { name: m.forceContentName, has_hls: true, has_dash: true }
    else if m.availableContent.count() > 0
        content = m.availableContent[m.currentContentIndex]
    else
        m.statusLabel.text = "No content to play"
        return
    end if
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

    print "Stream URL: "; streamUrl
    
    ' Update URL display
    m.urlValue.text = streamUrl
    
    ' Play the stream
    playStream(streamUrl, content.name)
end sub

function buildStreamURL(env as Object, contentName as String, protocol as String, segment as String) as String
    baseUrl = "http://" + env.host + ":" + env.playbackPort
    path = "/go-live/" + contentName + "/"
    
    path = path + "manifest_2s.mpd"
    
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
    return "dash"
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
