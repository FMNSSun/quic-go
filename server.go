package quic

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/lucas-clemente/quic-go/crypto"
	"github.com/lucas-clemente/quic-go/handshake"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/qerr"
	"github.com/lucas-clemente/quic-go/utils"
   "github.com/mami-project/plus-lib"
   "fmt"
)

// packetHandler handles packets
type packetHandler interface {
	Session
	handlePacket(*receivedPacket)
	run() error
}

// A Listener of QUIC
type server struct {
	config *Config

	conn net.PacketConn
    plusConnManager *PLUS.ConnectionManager

	certChain crypto.CertChain
	scfg      *handshake.ServerConfig

	sessions                  map[protocol.ConnectionID]packetHandler
	sessionsMutex             sync.RWMutex
	deleteClosedSessionsAfter time.Duration

	serverError  error
	sessionQueue chan Session
	errorChan    chan struct{}

	newSession func(conn connection, v protocol.VersionNumber, connectionID protocol.ConnectionID, sCfg *handshake.ServerConfig, config *Config, plusConnection *PLUS.Connection) (packetHandler, <-chan handshakeEvent, error)
}

var _ Listener = &server{}

// ListenAddr creates a QUIC server listening on a given address.
// The listener is not active until Serve() is called.
func ListenAddr(addr string, config *Config) (Listener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	return Listen(conn, config)
}

// Listen listens for QUIC connections on a given net.PacketConn.
// The listener is not active until Serve() is called.
func Listen(conn net.PacketConn, config *Config) (Listener, error) {
	certChain := crypto.NewCertChain(config.TLSConfig)
	kex, err := crypto.NewCurve25519KEX()
	if err != nil {
		return nil, err
	}
	scfg, err := handshake.NewServerConfig(kex, certChain)
	if err != nil {
		return nil, err
	}

    var s *server

    if !config.UsePLUS {
        s = &server{
            conn:                      conn,
            plusConnManager:           nil,
            config:                    populateServerConfig(config),
            certChain:                 certChain,
            scfg:                      scfg,
            sessions:                  map[protocol.ConnectionID]packetHandler{},
            newSession:                newSession,
            deleteClosedSessionsAfter: protocol.ClosedSessionDeleteTimeout,
            sessionQueue:              make(chan Session, 5),
            errorChan:                 make(chan struct{}),
        }
    } else {
        s = &server{
            conn:                      nil,
            plusConnManager:           PLUS.NewConnectionManager(conn),
            config:                    populateServerConfig(config),
            certChain:                 certChain,
            scfg:                      scfg,
            sessions:                  map[protocol.ConnectionID]packetHandler{},
            newSession:                newSession,
            deleteClosedSessionsAfter: protocol.ClosedSessionDeleteTimeout,
            sessionQueue:              make(chan Session, 5),
            errorChan:                 make(chan struct{}),
        }
    }
	go s.serve()
	return s, nil
}

func populateServerConfig(config *Config) *Config {
	versions := config.Versions
	if len(versions) == 0 {
		versions = protocol.SupportedVersions
	}

	return &Config{
		TLSConfig: config.TLSConfig,
		Versions:  versions,
        UsePLUS:  config.UsePLUS,
	}
}

// serve with PLUS
func (s *server) servePLUS() {
    fmt.Println("servePLUS")
    for {
        plusConnection, plusPacket, remoteAddr, feedbackData, err := s.plusConnManager.ReadAndProcessPacket()

        if err != nil {
            s.serverError = err
            close(s.errorChan)
            _ = s.Close()
            return
        }

		  data := getPacketBuffer()
		  data = data[:protocol.MaxReceivePacketSize]

		  payload := plusPacket.Payload()
		  
        copy(data, payload)
		  //fmt.Printf("len(data) := %d, len(payload) := %d, cap(data) := %d\n", len(data), len(payload), cap(data))
		  data = data[:len(payload)]

		  s.plusConnManager.ReturnPacketAndBuffer(plusPacket)

        if feedbackData != nil {
			utils.Infof("Have to send feedback data %x", feedbackData)
		}

        if err := s.handlePacketPLUS(s.conn, remoteAddr, data, plusConnection, feedbackData); err != nil {
            fmt.Printf("error handling PLUS packet: %s\n", err.Error())
            utils.Errorf("error handling PLUS packet: %s", err.Error())
        }
    }
}

// serve listens on an existing PacketConn
func (s *server) serve() {
    if(s.config.UsePLUS) {
        s.servePLUS()
        return
    }

	for {
		data := getPacketBuffer()
		data = data[:protocol.MaxReceivePacketSize]
		// The packet size should not exceed protocol.MaxReceivePacketSize bytes
		// If it does, we only read a truncated packet, which will then end up undecryptable
		n, remoteAddr, err := s.conn.ReadFrom(data)
		if err != nil {
			s.serverError = err
			close(s.errorChan)
			_ = s.Close()
			return
		}
		data = data[:n]
		if err := s.handlePacket(s.conn, remoteAddr, data); err != nil {
			utils.Errorf("error handling packet: %s", err.Error())
		}
	}
}

// Accept returns newly openend sessions
func (s *server) Accept() (Session, error) {
	var sess Session
	select {
	case sess = <-s.sessionQueue:
		return sess, nil
	case <-s.errorChan:
		return nil, s.serverError
	}
}

// Close the server
func (s *server) Close() error {
	s.sessionsMutex.Lock()
	for _, session := range s.sessions {
		if session != nil {
			s.sessionsMutex.Unlock()
			_ = session.Close(nil)
			s.sessionsMutex.Lock()
		}
	}
	s.sessionsMutex.Unlock()

	if s.conn == nil {
		return nil
	}

    if !s.config.UsePLUS {
        return s.conn.Close()
    } else {
        return s.plusConnManager.Close()
    }
}

// Addr returns the server's network address
func (s *server) Addr() net.Addr {
    if !s.config.UsePLUS {
        return s.conn.LocalAddr()
    } else {
        return s.plusConnManager.LocalAddr()
    }
}

func (s *server) writeTo(pconn net.PacketConn, data []byte, remoteAddr net.Addr) error {
    if pconn == nil {
        pconn = s.conn
    }

    if(!s.config.UsePLUS) {
        _, err := pconn.WriteTo(data, remoteAddr)
        return err
    } else {
        return nil
    }
}

func (s *server) writeToPLUS(plusConnection *PLUS.Connection, data []byte) error {
    fmt.Println("server.go: writeToPLUS")

    if(s.config.UsePLUS) {
        _, err := plusConnection.Write(data)
        return err
    } else {
        return nil
    }
}

func (s *server) handlePacket(pconn net.PacketConn, remoteAddr net.Addr, packet []byte) error {
    return s.handlePacketPLUS(pconn, remoteAddr, packet, nil, nil)
}

func (s *server) handlePacketPLUS(pconn net.PacketConn, remoteAddr net.Addr, packet []byte, plusConnection *PLUS.Connection, feedbackData []byte) error {
	rcvTime := time.Now()

	r := bytes.NewReader(packet)
	hdr, err := ParsePublicHeader(r, protocol.PerspectiveClient)
	if err != nil {
		return qerr.Error(qerr.InvalidPacketHeader, err.Error())
	}
	hdr.Raw = packet[:len(packet)-r.Len()]

	s.sessionsMutex.RLock()
	session, ok := s.sessions[hdr.ConnectionID]
	s.sessionsMutex.RUnlock()

	// ignore all Public Reset packets
	if hdr.ResetFlag {
		if ok {
			var pr *publicReset
			pr, err = parsePublicReset(r)
			if err != nil {
				utils.Infof("Received a Public Reset for connection %x. An error occurred parsing the packet.")
			} else {
				utils.Infof("Received a Public Reset for connection %x, rejected packet number: 0x%x.", hdr.ConnectionID, pr.rejectedPacketNumber)
			}
		} else {
			utils.Infof("Received Public Reset for unknown connection %x.", hdr.ConnectionID)
		}
		return nil
	}

	// a session is only created once the client sent a supported version
	// if we receive a packet for a connection that already has session, it's probably an old packet that was sent by the client before the version was negotiated
	// it is safe to drop it
	if ok && hdr.VersionFlag && !protocol.IsSupportedVersion(s.config.Versions, hdr.VersionNumber) {
		return nil
	}

	// Send Version Negotiation Packet if the client is speaking a different protocol version
	if hdr.VersionFlag && !protocol.IsSupportedVersion(s.config.Versions, hdr.VersionNumber) {
		// drop packets that are too small to be valid first packets
		if len(packet) < protocol.ClientHelloMinimumSize+len(hdr.Raw) {
			return errors.New("dropping small packet with unknown version")
		}
		utils.Infof("Client offered version %d, sending VersionNegotiationPacket", hdr.VersionNumber)
        if !s.config.UsePLUS {
            err = s.writeTo(pconn, composeVersionNegotiation(hdr.ConnectionID, s.config.Versions), remoteAddr)
        } else {
            err = s.writeToPLUS(plusConnection, composeVersionNegotiation(hdr.ConnectionID, s.config.Versions))
        }
		return err
	}

	if !ok {
		if !hdr.VersionFlag {
            if !s.config.UsePLUS {
                err = s.writeTo(pconn, writePublicReset(hdr.ConnectionID, hdr.PacketNumber, 0), remoteAddr)
            } else {
                err = s.writeToPLUS(plusConnection, composeVersionNegotiation(hdr.ConnectionID, s.config.Versions))
            }
			return err
		}
		version := hdr.VersionNumber
		if !protocol.IsSupportedVersion(s.config.Versions, version) {
			return errors.New("Server BUG: negotiated version not supported")
		}

		utils.Infof("Serving new connection: %x, version %d from %v", hdr.ConnectionID, version, remoteAddr)
		var handshakeChan <-chan handshakeEvent

		session, handshakeChan, err = s.newSession(
			&conn{pconn: pconn, currentAddr: remoteAddr},
			version,
			hdr.ConnectionID,
			s.scfg,
			s.config,
            plusConnection,
		)
		if err != nil {
			return err
		}
		s.sessionsMutex.Lock()
		s.sessions[hdr.ConnectionID] = session
		s.sessionsMutex.Unlock()

		go func() {
			// session.run() returns as soon as the session is closed
			_ = session.run()
			s.removeConnection(hdr.ConnectionID)
		}()

		go func() {
			for {
				ev := <-handshakeChan
				if ev.err != nil {
					return
				}
				if ev.encLevel == protocol.EncryptionForwardSecure {
					break
				}
			}
			s.sessionQueue <- session
		}()
	}
	if session == nil {
		// Late packet for closed session
		return nil
	}
	session.handlePacket(&receivedPacket{
		remoteAddr:   remoteAddr,
		publicHeader: hdr,
		data:         packet[len(packet)-r.Len():],
		rcvTime:      rcvTime,
		feedbackData: feedbackData,
	})
	return nil
}

func (s *server) removeConnection(id protocol.ConnectionID) {
	s.sessionsMutex.Lock()
	s.sessions[id] = nil
	s.sessionsMutex.Unlock()

	time.AfterFunc(s.deleteClosedSessionsAfter, func() {
		s.sessionsMutex.Lock()
		delete(s.sessions, id)
		s.sessionsMutex.Unlock()
	})
}

func composeVersionNegotiation(connectionID protocol.ConnectionID, versions []protocol.VersionNumber) []byte {
	fullReply := &bytes.Buffer{}
	responsePublicHeader := PublicHeader{
		ConnectionID: connectionID,
		PacketNumber: 1,
		VersionFlag:  true,
	}
	err := responsePublicHeader.Write(fullReply, protocol.VersionWhatever, protocol.PerspectiveServer)
	if err != nil {
		utils.Errorf("error composing version negotiation packet: %s", err.Error())
	}
	for _, v := range versions {
		utils.WriteUint32(fullReply, protocol.VersionNumberToTag(v))
	}
	return fullReply.Bytes()
}
