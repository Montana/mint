package mint

import (
	"bytes"
	"fmt"
)

type Capabilities struct {
	// For both client and server
	CipherSuites     []CipherSuite
	Groups           []NamedGroup
	SignatureSchemes []SignatureScheme
	PSKs             map[string]PreSharedKey

	// For client
	PSKModes []PSKKeyExchangeMode

	// For server
	NextProtos     []string
	Certificates   []*Certificate
	AllowEarlyData bool
}

type ConnectionOptions struct {
	ServerName string
	NextProtos []string
	EarlyData  []byte
}

type ConnectionParameters struct {
	UsingPSK       bool
	UsingDH        bool
	UsingEarlyData bool

	CipherSuite CipherSuite
	ServerName  string
	NextProto   string
}

type Handshake interface {
	IsClient() bool
	ConnectionParams() ConnectionParameters
	CryptoContext() *cryptoContext
	InboundKeys() (aeadFactory, keySet)
	OutboundKeys() (aeadFactory, keySet)
	CreateKeyUpdate(KeyUpdateRequest) (*HandshakeMessage, error)
	HandleKeyUpdate(*HandshakeMessage) (*HandshakeMessage, error)
	HandleNewSessionTicket(*HandshakeMessage) (PreSharedKey, error)
}

///// Common methods

func createKeyUpdate(client bool, ctx *cryptoContext, requestUpdate KeyUpdateRequest) (*HandshakeMessage, error) {
	// Roll the outbound keys
	err := ctx.updateKeys(client)
	if err != nil {
		return nil, err
	}

	// Return a KeyUpdate message
	return HandshakeMessageFromBody(&KeyUpdateBody{
		KeyUpdateRequest: requestUpdate,
	})
}

func handleKeyUpdate(client bool, ctx *cryptoContext, hm *HandshakeMessage) (*HandshakeMessage, error) {
	var ku KeyUpdateBody
	_, err := ku.Unmarshal(hm.body)
	if err != nil {
		return nil, err
	}

	// Roll the inbound keys
	err = ctx.updateKeys(!client)
	if err != nil {
		return nil, err
	}

	// If requested, roll outbound keys and send a KeyUpdate
	var outboundMessage *HandshakeMessage
	if ku.KeyUpdateRequest == KeyUpdateRequested {
		err = ctx.updateKeys(client)
		if err != nil {
			return nil, err
		}

		return HandshakeMessageFromBody(&KeyUpdateBody{
			KeyUpdateRequest: KeyUpdateNotRequested,
		})
	}

	return outboundMessage, nil
}

///// Client-side Handshake methods

type ClientHandshake struct {
	OfferedDH  map[NamedGroup][]byte
	OfferedPSK PreSharedKey

	PSK     []byte
	Context cryptoContext
	Params  ConnectionParameters

	AuthCertificate func(chain []CertificateEntry) error

	clientHello *HandshakeMessage
	serverHello *HandshakeMessage
}

func (h *ClientHandshake) IsClient() bool {
	return true
}

func (h *ClientHandshake) CryptoContext() *cryptoContext {
	return &h.Context
}

func (h ClientHandshake) ConnectionParams() ConnectionParameters {
	return h.Params
}

func (h *ClientHandshake) InboundKeys() (aeadFactory, keySet) {
	return h.Context.params.cipher, h.Context.serverTrafficKeys
}

func (h *ClientHandshake) OutboundKeys() (aeadFactory, keySet) {
	return h.Context.params.cipher, h.Context.clientTrafficKeys
}

func (h *ClientHandshake) CreateKeyUpdate(requestUpdate KeyUpdateRequest) (*HandshakeMessage, error) {
	return createKeyUpdate(true, &h.Context, requestUpdate)
}

func (h *ClientHandshake) HandleKeyUpdate(hm *HandshakeMessage) (*HandshakeMessage, error) {
	return handleKeyUpdate(true, &h.Context, hm)
}

func (h *ClientHandshake) HandleNewSessionTicket(hm *HandshakeMessage) (PreSharedKey, error) {
	var tkt NewSessionTicketBody
	_, err := tkt.Unmarshal(hm.body)
	if err != nil {
		return PreSharedKey{}, err
	}

	psk := PreSharedKey{
		CipherSuite:  h.Context.suite,
		IsResumption: true,
		Identity:     tkt.Ticket,
		Key:          h.Context.resumptionSecret,
	}

	return psk, nil
}

func (h *ClientHandshake) CreateClientHello(opts ConnectionOptions, caps Capabilities) (*HandshakeMessage, error) {
	// key_shares
	h.OfferedDH = map[NamedGroup][]byte{}
	ks := KeyShareExtension{
		HandshakeType: HandshakeTypeClientHello,
		Shares:        make([]KeyShareEntry, len(caps.Groups)),
	}
	for i, group := range caps.Groups {
		pub, priv, err := newKeyShare(group)
		if err != nil {
			return nil, err
		}

		ks.Shares[i].Group = group
		ks.Shares[i].KeyExchange = pub
		h.OfferedDH[group] = priv
	}

	// supported_versions, supported_groups, signature_algorithms, server_name
	sv := SupportedVersionsExtension{Versions: []uint16{supportedVersion}}
	sni := ServerNameExtension(opts.ServerName)
	sg := SupportedGroupsExtension{Groups: caps.Groups}
	sa := SignatureAlgorithmsExtension{Algorithms: caps.SignatureSchemes}
	kem := PSKKeyExchangeModesExtension{KEModes: caps.PSKModes}

	h.Params.ServerName = opts.ServerName

	// Application Layer Protocol Negotiation
	var alpn *ALPNExtension
	if (opts.NextProtos != nil) && (len(opts.NextProtos) > 0) {
		alpn = &ALPNExtension{Protocols: opts.NextProtos}
	}

	// Construct base ClientHello
	ch := &ClientHelloBody{
		CipherSuites: caps.CipherSuites,
	}
	_, err := prng.Read(ch.Random[:])
	if err != nil {
		return nil, err
	}
	for _, ext := range []ExtensionBody{&sv, &sni, &ks, &sg, &sa, &kem} {
		err := ch.Extensions.Add(ext)
		if err != nil {
			return nil, err
		}
	}
	if alpn != nil {
		// XXX: This can't be folded into the above because Go interface-typed
		// values are never reported as nil
		err := ch.Extensions.Add(alpn)
		if err != nil {
			return nil, err
		}
	}

	// Handle PSK and EarlyData just before transmitting, so that we can
	// calculate the PSK binder value
	var psk *PreSharedKeyExtension
	var ed *EarlyDataExtension
	if key, ok := caps.PSKs[opts.ServerName]; ok {
		h.OfferedPSK = key

		// Narrow ciphersuites to ones that match PSK hash
		keyParams, ok := cipherSuiteMap[key.CipherSuite]
		if !ok {
			return nil, fmt.Errorf("Unsupported ciphersuite from PSK")
		}

		compatibleSuites := []CipherSuite{}
		for _, suite := range ch.CipherSuites {
			if cipherSuiteMap[suite].hash == keyParams.hash {
				compatibleSuites = append(compatibleSuites, suite)
			}
		}
		ch.CipherSuites = compatibleSuites

		// Signal early data if we're going to do it
		if opts.EarlyData != nil {
			ed = &EarlyDataExtension{}
			ch.Extensions.Add(ed)
		}

		// Add the shim PSK extension to the ClientHello
		psk = &PreSharedKeyExtension{
			HandshakeType: HandshakeTypeClientHello,
			Identities: []PSKIdentity{
				PSKIdentity{Identity: key.Identity},
			},
			Binders: []PSKBinderEntry{
				// Note: Stub to get the length fields right
				PSKBinderEntry{Binder: bytes.Repeat([]byte{0x00}, keyParams.hash.Size())},
			},
		}
		ch.Extensions.Add(psk)

		// Pre-Initialize the crypto context and compute the binder value
		h.Context.preInit(key)

		// Compute the binder value
		trunc, err := ch.Truncated()
		if err != nil {
			return nil, err
		}

		truncHash := h.Context.params.hash.New()
		truncHash.Write(trunc)

		binder := h.Context.computeFinishedData(h.Context.binderKey, truncHash.Sum(nil))

		// Replace the PSK extension
		psk.Binders[0].Binder = binder
		ch.Extensions.Add(psk)

		h.clientHello, err = HandshakeMessageFromBody(ch)
		if err != nil {
			return nil, err
		}

		h.Context.earlyUpdateWithClientHello(h.clientHello)
	}

	h.clientHello, err = HandshakeMessageFromBody(ch)
	if err != nil {
		return nil, err
	}

	return h.clientHello, nil
}

func (h *ClientHandshake) HandleServerHello(shm *HandshakeMessage) error {
	// Unmarshal the ServerHello
	sh := &ServerHelloBody{}
	_, err := sh.Unmarshal(shm.body)
	if err != nil {
		return err
	}

	// Check that the version sent by the server is the one we support
	if sh.Version != supportedVersion {
		return fmt.Errorf("tls.client: Server sent unsupported version %x", sh.Version)
	}

	// Do PSK or key agreement depending on extensions
	serverPSK := PreSharedKeyExtension{HandshakeType: HandshakeTypeServerHello}
	serverKeyShare := KeyShareExtension{HandshakeType: HandshakeTypeServerHello}
	serverEarlyData := EarlyDataExtension{}

	foundPSK := sh.Extensions.Find(&serverPSK)
	foundKeyShare := sh.Extensions.Find(&serverKeyShare)
	h.Params.UsingEarlyData = sh.Extensions.Find(&serverEarlyData)

	if foundPSK && (serverPSK.SelectedIdentity == 0) {
		h.PSK = h.OfferedPSK.Key
		h.Params.UsingPSK = true
		logf(logTypeHandshake, "[client] got PSK extension")
	} else {
		// If the server rejected our PSK, then we have to re-start without it
		h.Context = cryptoContext{}
	}

	var dhSecret []byte
	if foundKeyShare {
		sks := serverKeyShare.Shares[0]
		priv, ok := h.OfferedDH[sks.Group]
		if !ok {
			return fmt.Errorf("Server key share for unknown group")
		}

		h.Params.UsingDH = true
		dhSecret, _ = keyAgreement(sks.Group, sks.KeyExchange, priv)
		logf(logTypeHandshake, "[client] got key share extension")
	}

	h.serverHello = shm

	err = h.Context.init(sh.CipherSuite, h.clientHello)
	if err != nil {
		return err
	}

	h.Context.updateWithServerHello(h.serverHello, dhSecret)
	return nil
}

func (h *ClientHandshake) HandleServerFirstFlight(transcript []*HandshakeMessage, finishedMessage *HandshakeMessage) error {
	// Extract messages from sequence
	var err error
	var ee *EncryptedExtensionsBody
	var cert *CertificateBody
	var certVerify *CertificateVerifyBody
	var certVerifyIndex int
	for i, msg := range transcript {
		switch msg.msgType {
		case HandshakeTypeEncryptedExtensions:
			ee = new(EncryptedExtensionsBody)
			_, err = ee.Unmarshal(msg.body)
		case HandshakeTypeCertificate:
			cert = new(CertificateBody)
			_, err = cert.Unmarshal(msg.body)
		case HandshakeTypeCertificateVerify:
			certVerifyIndex = i
			certVerify = new(CertificateVerifyBody)
			_, err = certVerify.Unmarshal(msg.body)
		}

		if err != nil {
			return err
		}
	}

	// Read data from EncryptedExtensions
	serverALPN := ALPNExtension{}
	serverEarlyData := EarlyDataExtension{}

	gotALPN := ee.Extensions.Find(&serverALPN)
	h.Params.UsingEarlyData = ee.Extensions.Find(&serverEarlyData)

	if gotALPN && len(serverALPN.Protocols) > 0 {
		h.Params.NextProto = serverALPN.Protocols[0]
	}

	// Verify the server's certificate if we're not using a PSK for authentication
	if h.PSK == nil {

		if cert == nil || certVerify == nil {
			return fmt.Errorf("tls.client: No server auth data provided")
		}

		transcriptForCertVerify := []*HandshakeMessage{h.clientHello, h.serverHello}
		transcriptForCertVerify = append(transcriptForCertVerify, transcript[:certVerifyIndex]...)
		logf(logTypeHandshake, "[client] Transcript for certVerify")
		for _, hm := range transcriptForCertVerify {
			logf(logTypeHandshake, "  [%d] %x", hm.msgType, hm.body)
		}
		logf(logTypeHandshake, "===")

		serverPublicKey := cert.CertificateList[0].CertData.PublicKey
		if err = certVerify.Verify(serverPublicKey, transcriptForCertVerify, h.Context); err != nil {
			return err
		}

		if h.AuthCertificate != nil {
			err = h.AuthCertificate(cert.CertificateList)
			if err != nil {
				return err
			}
		}
	}

	h.Context.updateWithServerFirstFlight(transcript)

	// Verify server finished
	sfin := new(FinishedBody)
	sfin.VerifyDataLen = h.Context.serverFinished.VerifyDataLen
	_, err = sfin.Unmarshal(finishedMessage.body)
	if err != nil {
		return err
	}
	if !bytes.Equal(sfin.VerifyData, h.Context.serverFinished.VerifyData) {
		return fmt.Errorf("tls.client: Server's Finished failed to verify")
	}

	return nil
}

///// Server-side handshake logic

type ServerHandshake struct {
	Context cryptoContext
	Params  ConnectionParameters
}

func (h *ServerHandshake) IsClient() bool {
	return true
}

func (h *ServerHandshake) CryptoContext() *cryptoContext {
	return &h.Context
}

func (h ServerHandshake) ConnectionParams() ConnectionParameters {
	return h.Params
}

func (h *ServerHandshake) CreateKeyUpdate(requestUpdate KeyUpdateRequest) (*HandshakeMessage, error) {
	return createKeyUpdate(false, &h.Context, requestUpdate)
}

func (h *ServerHandshake) HandleKeyUpdate(hm *HandshakeMessage) (*HandshakeMessage, error) {
	return handleKeyUpdate(false, &h.Context, hm)
}

func (h *ServerHandshake) HandleNewSessionTicket(hm *HandshakeMessage) (PreSharedKey, error) {
	return PreSharedKey{}, fmt.Errorf("tls.server: Client sent NewSessionTicket")
}

func (h *ServerHandshake) InboundKeys() (aeadFactory, keySet) {
	return h.Context.params.cipher, h.Context.clientTrafficKeys
}

func (h *ServerHandshake) OutboundKeys() (aeadFactory, keySet) {
	return h.Context.params.cipher, h.Context.serverTrafficKeys
}

func (h *ServerHandshake) HandleClientHello(chm *HandshakeMessage, caps Capabilities) (*HandshakeMessage, []*HandshakeMessage, error) {
	ch := &ClientHelloBody{}
	_, err := ch.Unmarshal(chm.body)
	if err != nil {
		return nil, nil, err
	}

	supportedVersions := new(SupportedVersionsExtension)
	serverName := new(ServerNameExtension)
	supportedGroups := new(SupportedGroupsExtension)
	signatureAlgorithms := new(SignatureAlgorithmsExtension)
	clientKeyShares := &KeyShareExtension{HandshakeType: HandshakeTypeClientHello}
	clientPSK := &PreSharedKeyExtension{HandshakeType: HandshakeTypeClientHello}
	clientEarlyData := &EarlyDataExtension{}
	clientALPN := new(ALPNExtension)
	clientPSKModes := new(PSKKeyExchangeModesExtension)

	gotSupportedVersions := ch.Extensions.Find(supportedVersions)
	gotServerName := ch.Extensions.Find(serverName)
	gotSupportedGroups := ch.Extensions.Find(supportedGroups)
	gotSignatureAlgorithms := ch.Extensions.Find(signatureAlgorithms)
	gotEarlyData := ch.Extensions.Find(clientEarlyData)
	ch.Extensions.Find(clientKeyShares)
	ch.Extensions.Find(clientPSK)
	ch.Extensions.Find(clientALPN)
	ch.Extensions.Find(clientPSKModes)

	if gotServerName {
		h.Params.ServerName = string(*serverName)
	}

	// If the client didn't send supportedVersions or doesn't support 1.3,
	// then we're done here.
	if !gotSupportedVersions {
		logf(logTypeHandshake, "[server] Client did not send supported_versions")
		return nil, nil, fmt.Errorf("tls.server: Client did not send supported_versions")
	}
	versionOK, _ := VersionNegotiation(supportedVersions.Versions, []uint16{supportedVersion})
	if !versionOK {
		logf(logTypeHandshake, "[server] Client does not support the same version")
		return nil, nil, fmt.Errorf("tls.server: Client does not support the same version")
	}

	// Figure out if we can do DH
	canDoDH, dhGroup, dhPub, dhSecret := DHNegotiation(clientKeyShares.Shares, caps.Groups)

	// Figure out if we can do PSK
	canDoPSK := false
	var selectedPSK int
	var psk *PreSharedKey
	var ctx cryptoContext
	if len(clientPSK.Identities) > 0 {
		chTrunc, err := ch.Truncated()
		if err != nil {
			return nil, nil, err
		}
		canDoPSK, selectedPSK, psk, ctx, err = PSKNegotiation(clientPSK.Identities, clientPSK.Binders, chTrunc, caps.PSKs)
		if err != nil {
			return nil, nil, err
		}
	}
	h.Context = ctx

	// Figure out if we actually should do DH / PSK
	h.Params.UsingDH, h.Params.UsingPSK = PSKModeNegotiation(canDoDH, canDoPSK, clientPSKModes.KEModes)

	// If we've got no entropy to make keys from, fail
	if !h.Params.UsingDH && !h.Params.UsingPSK {
		logf(logTypeHandshake, "[server] Neither DH nor PSK negotiated")
		return nil, nil, fmt.Errorf("Neither DH nor PSK negotiated")
	}

	var cert *Certificate
	var certScheme SignatureScheme
	if !h.Params.UsingPSK {
		psk = nil
		h.Context = cryptoContext{}

		// If we're not using a PSK mode, then we need to have certain extensions
		if !gotServerName || !gotSupportedGroups || !gotSignatureAlgorithms {
			logf(logTypeHandshake, "[server] Insufficient extensions (%v %v %v)",
				gotServerName, gotSupportedGroups, gotSignatureAlgorithms)
			return nil, nil, fmt.Errorf("tls.server: Missing extension in ClientHello")
		}

		// Select a certificate
		cert, certScheme, err = CertificateSelection(string(*serverName), signatureAlgorithms.Algorithms, caps.Certificates)
	}

	if !h.Params.UsingDH {
		dhSecret = nil
	}

	// Figure out if we're going to do early data
	h.Params.UsingEarlyData = EarlyDataNegotiation(h.Params.UsingPSK, gotEarlyData, caps.AllowEarlyData)

	if h.Params.UsingEarlyData {
		h.Context.earlyUpdateWithClientHello(chm)
	}

	// Select a ciphersuite
	chosenSuite, err := CipherSuiteNegotiation(psk, ch.CipherSuites, caps.CipherSuites)
	if err != nil {
		return nil, nil, err
	}

	// Select a next protocol
	h.Params.NextProto, err = ALPNNegotiation(psk, clientALPN.Protocols, caps.NextProtos)
	if err != nil {
		return nil, nil, err
	}

	// Create the ServerHello
	sh := &ServerHelloBody{
		Version:     supportedVersion,
		CipherSuite: chosenSuite,
	}
	_, err = prng.Read(sh.Random[:])
	if err != nil {
		return nil, nil, err
	}
	if h.Params.UsingDH {
		logf(logTypeHandshake, "[server] sending DH extension")
		err = sh.Extensions.Add(&KeyShareExtension{
			HandshakeType: HandshakeTypeServerHello,
			Shares:        []KeyShareEntry{KeyShareEntry{Group: dhGroup, KeyExchange: dhPub}},
		})
		if err != nil {
			return nil, nil, err
		}
	}
	if h.Params.UsingPSK {
		logf(logTypeHandshake, "[server] sending PSK extension")
		err = sh.Extensions.Add(&PreSharedKeyExtension{
			HandshakeType:    HandshakeTypeServerHello,
			SelectedIdentity: uint16(selectedPSK),
		})
		if err != nil {
			return nil, nil, err
		}
	}
	logf(logTypeHandshake, "[server] Done creating ServerHello")

	shm, err := HandshakeMessageFromBody(sh)
	if err != nil {
		return nil, nil, err
	}

	// Crank up the crypto context
	err = h.Context.init(sh.CipherSuite, chm)
	if err != nil {
		return nil, nil, err
	}

	err = h.Context.updateWithServerHello(shm, dhSecret)
	if err != nil {
		return nil, nil, err
	}

	// Send an EncryptedExtensions message (even if it's empty)
	eeList := ExtensionList{}
	if h.Params.NextProto != "" {
		logf(logTypeHandshake, "[server] sending ALPN extension")
		err = eeList.Add(&ALPNExtension{Protocols: []string{h.Params.NextProto}})
		if err != nil {
			return nil, nil, err
		}
	}
	if h.Params.UsingEarlyData {
		logf(logTypeHandshake, "[server] sending EDI extension")
		err = eeList.Add(&EarlyDataExtension{})
		if err != nil {
			return nil, nil, err
		}
	}
	ee := &EncryptedExtensionsBody{eeList}
	eem, err := HandshakeMessageFromBody(ee)
	if err != nil {
		return nil, nil, err
	}

	transcript := []*HandshakeMessage{eem}

	// Authenticate with a certificate if required
	if !h.Params.UsingPSK {
		// Create and send Certificate, CertificateVerify
		certificate := &CertificateBody{
			CertificateList: make([]CertificateEntry, len(cert.Chain)),
		}
		for i, entry := range cert.Chain {
			certificate.CertificateList[i] = CertificateEntry{CertData: entry}
		}
		certm, err := HandshakeMessageFromBody(certificate)
		if err != nil {
			return nil, nil, err
		}

		certificateVerify := &CertificateVerifyBody{Algorithm: certScheme}
		logf(logTypeHandshake, "Creating CertVerify: %04x %v", certScheme, h.Context.params.hash)
		err = certificateVerify.Sign(cert.PrivateKey, []*HandshakeMessage{chm, shm, eem, certm}, h.Context)
		if err != nil {
			return nil, nil, err
		}
		certvm, err := HandshakeMessageFromBody(certificateVerify)
		if err != nil {
			return nil, nil, err
		}

		transcript = append(transcript, []*HandshakeMessage{certm, certvm}...)
	}

	// Crank the crypto context
	err = h.Context.updateWithServerFirstFlight(transcript)
	if err != nil {
		return nil, nil, err
	}
	fm, err := HandshakeMessageFromBody(h.Context.serverFinished)
	if err != nil {
		return nil, nil, err
	}

	transcript = append(transcript, fm)

	return shm, transcript, nil
}

func (h *ServerHandshake) HandleClientSecondFlight(transcript []*HandshakeMessage, finishedMessage *HandshakeMessage) error {
	// XXX Currently, we don't process anything besides the Finished

	err := h.Context.updateWithClientSecondFlight(transcript)
	if err != nil {
		return err
	}

	// Read and verify client Finished
	cfin := new(FinishedBody)
	cfin.VerifyDataLen = h.Context.clientFinished.VerifyDataLen
	_, err = cfin.Unmarshal(finishedMessage.body)
	if err != nil {
		return err
	}
	if !bytes.Equal(cfin.VerifyData, h.Context.clientFinished.VerifyData) {
		return fmt.Errorf("tls.server: Client's Finished failed to verify")
	}

	return nil
}

func (h *ServerHandshake) CreateNewSessionTicket(length int, lifetime uint32) (PreSharedKey, *HandshakeMessage, error) {
	// TODO: Check that we're in the right state for this

	tkt, err := NewSessionTicket(length)
	if err != nil {
		return PreSharedKey{}, nil, err
	}

	tkt.TicketLifetime = lifetime

	err = tkt.Extensions.Add(&TicketEarlyDataInfoExtension{1 << 24})
	if err != nil {
		return PreSharedKey{}, nil, err
	}

	newPSK := PreSharedKey{
		CipherSuite:  h.Context.suite,
		IsResumption: true,
		Identity:     tkt.Ticket,
		Key:          h.Context.resumptionSecret,
	}

	tktm, err := HandshakeMessageFromBody(tkt)
	return newPSK, tktm, err
}
