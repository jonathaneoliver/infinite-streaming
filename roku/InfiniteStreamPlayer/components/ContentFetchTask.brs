sub init()
    m.top.functionName = "run"
end sub

sub run()
    m.top.response = ""
    m.top.error = ""
    m.top.statusCode = 0

    if m.top.url = "" or m.top.url = invalid then
        m.top.error = "Missing URL"
        return
    end if

    request = CreateObject("roUrlTransfer")
    request.SetUrl(m.top.url)
    request.SetCertificatesFile("common:/certs/ca-bundle.crt")
    request.InitClientCertificates()
    request.EnablePeerVerification(false)
    request.EnableHostVerification(false)

    responseString = request.GetToString()
    statusCode = 0

    if responseString = invalid then
        m.top.error = "Request failed"
        m.top.statusCode = statusCode
        return
    end if

    m.top.statusCode = 200
    m.top.response = responseString
end sub
