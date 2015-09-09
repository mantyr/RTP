package proxies

// Converts AMP to RTSP

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/antongulenko/RTP/protocols"
	"github.com/antongulenko/RTP/protocols/amp"
	"github.com/antongulenko/RTP/rtpClient"
)

const (
	minPort = 20000
	maxPort = 50000
)

type AmpProxy struct {
	*protocols.Server

	rtspURL   *url.URL
	proxyHost string
	sessions  map[string]*proxySession

	RtspStartedCallback func(rtsp *rtpClient.RtspClient)
	RtspEndedCallback   func(rtsp *rtpClient.RtspClient)
}

type proxySession struct {
	backend   *rtpClient.RtspClient
	rtpProxy  *UdpProxy
	rtcpProxy *UdpProxy
	port      int
	mediaFile string
	client    string
}

// ampAddr: address to listen on for AMP requests
// rtspURL: base URL used when sending RTSP requests to the backend media server
// localProxyIP: address to receive RTP/RTCP packets from the media server
func NewAmpProxy(ampAddr, rtspURL, localProxyIP string) (*AmpProxy, error) {
	u, err := url.Parse(rtspURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "rtsp" {
		return nil, errors.New("Need rtsp:// rtspURL for AmpProxy")
	}

	ip, err := net.ResolveIPAddr("ip", localProxyIP)
	if err != nil {
		return nil, fmt.Errorf("Failed to resolve IP address %v: %v", localProxyIP, err)
	}

	proxy := &AmpProxy{
		rtspURL:   u,
		proxyHost: ip.String(),
		sessions:  make(map[string]*proxySession),
	}
	proxy.Server, err = protocols.NewServer(ampAddr, proxy)
	if err != nil {
		return nil, err
	}
	return proxy, nil
}

func (proxy *AmpProxy) StopServer() {
	for _, session := range proxy.sessions {
		proxy.cleanupSession(session)
	}
}

func (proxy *AmpProxy) ReceivePacket(conn *net.UDPConn) (*protocols.Packet, error) {
	packet, err := amp.ReceivePacket(conn)
	if err != nil {
		return nil, err
	}
	return packet.Packet, err
}

func (proxy *AmpProxy) HandleRequest(request *protocols.Packet) {
	packet := &amp.AmpPacket{request}
	switch packet.Code {
	case amp.CodeStartSession:
		if desc := packet.StartSession(); desc == nil {
			proxy.ReplyError(packet.Packet, fmt.Errorf("Illegal value for AMP CodeStartSession: %v", packet.Val))
		} else {
			proxy.ReplyCheck(packet.Packet, proxy.startSession(desc))
		}
	case amp.CodeStopSession:
		if desc := packet.StopSession(); desc == nil {
			proxy.ReplyError(packet.Packet, fmt.Errorf("Illegal value for AMP CodeStopSession: %v", packet.Val))
		} else {
			proxy.ReplyCheck(packet.Packet, proxy.stopSession(desc))
		}
	default:
		proxy.LogError(fmt.Errorf("Received unexpected AMP code: %v", packet.Code))
	}
}

func (proxy *AmpProxy) startSession(desc *amp.StartSessionValue) error {
	client := net.JoinHostPort(desc.ReceiverHost, strconv.Itoa(desc.Port))
	_, ok := proxy.sessions[client]
	if ok {
		return fmt.Errorf("Session already exists for client %v", client)
	}

	rtcpClient := net.JoinHostPort(desc.ReceiverHost, strconv.Itoa(desc.Port+1))
	rtpProxy, rtcpProxy, err := NewUdpProxyPair(proxy.proxyHost, client, rtcpClient, minPort, maxPort)
	if err != nil {
		return err
	}
	rtpProxy.Start()
	rtcpProxy.Start()
	rtpPort := rtpProxy.listenAddr.Port

	mediaURL := proxy.rtspURL.ResolveReference(&url.URL{Path: desc.MediaFile})
	logfile := fmt.Sprintf("amp-proxy-%v-%v.log", rtpPort, desc.MediaFile)
	rtsp, err := rtpClient.StartRtspClient(mediaURL.String(), rtpPort, logfile)
	if err != nil {
		return fmt.Errorf("Failed to start RTSP client: %v", err)
	}
	session := &proxySession{
		backend:   rtsp,
		mediaFile: desc.MediaFile,
		port:      desc.Port,
		rtpProxy:  rtpProxy,
		rtcpProxy: rtcpProxy,
		client:    client,
	}
	proxy.sessions[client] = session
	proxy.observe(session)
	return nil
}

func (proxy *AmpProxy) stopSession(desc *amp.StopSessionValue) error {
	client := net.JoinHostPort(desc.ReceiverHost, strconv.Itoa(desc.Port))
	session, ok := proxy.sessions[client]
	if !ok {
		return fmt.Errorf("Session not found for client %v", client)
	}
	proxy.cleanupSession(session)
	return nil
}

func (proxy *AmpProxy) cleanupSession(session *proxySession) {
	session.backend.Stop()
	session.rtpProxy.Close()
	session.rtcpProxy.Close()
	delete(proxy.sessions, session.client)
}

func (proxy *AmpProxy) observe(session *proxySession) {
	rtsp := session.backend
	if proxy.RtspStartedCallback != nil {
		proxy.RtspStartedCallback(rtsp)
	}
	c := rtsp.Observe(proxy.Wg)
	go func() {
		<-c
		if proxy.RtspEndedCallback != nil {
			proxy.RtspEndedCallback(rtsp)
		}
		proxy.cleanupSession(session)
	}()
}