package rtc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/go-logr/logr"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/config"
	serverlogger "github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
)

const (
	negotiationFrequency       = 150 * time.Millisecond
	negotiationFailedTimout    = 15 * time.Second
	dtlsRetransmissionInterval = 100 * time.Millisecond

	iceDisconnectedTimeout = 10 * time.Second // compatible for ice-lite with firefox client
	iceFailedTimeout       = 25 * time.Second // pion's default
	iceKeepaliveInterval   = 2 * time.Second  // pion's default

	shortConnectionThreshold = 2 * time.Minute
)

var (
	ErrIceRestartWithoutLocalSDP = errors.New("ICE restart without local SDP settled")
)

const (
	negotiationStateNone = iota
	// waiting for client answer
	negotiationStateClient
	// need to Negotiate again
	negotiationRetry
)

type SimulcastTrackInfo struct {
	Mid string
	Rid string
}

// PCTransport is a wrapper around PeerConnection, with some helper methods
type PCTransport struct {
	params TransportParams
	pc     *webrtc.PeerConnection
	me     *webrtc.MediaEngine

	lock                       sync.RWMutex
	iceConnectedAt             time.Time
	pendingCandidates          []webrtc.ICECandidateInit
	debouncedNegotiate         func(func())
	negotiationPending         map[livekit.ParticipantID]bool
	onOffer                    func(offer webrtc.SessionDescription)
	onRemoteDescripitonSettled func() error
	restartAfterGathering      bool
	restartAtNextOffer         bool
	negotiationState           int
	negotiateCounter           atomic.Int32
	signalStateCheckTimer      *time.Timer
	onNegotiationFailed        func()

	// stream allocator for subscriber PC
	streamAllocator *sfu.StreamAllocator

	previousAnswer *webrtc.SessionDescription

	preferTCP bool

	currentOfferIceCredential string // ice user:pwd, for publish side ice restart checking
	pendingRestartIceOffer    *webrtc.SessionDescription
}

type TransportParams struct {
	ParticipantID           livekit.ParticipantID
	ParticipantIdentity     livekit.ParticipantIdentity
	ProtocolVersion         types.ProtocolVersion
	Target                  livekit.SignalTarget
	Config                  *WebRTCConfig
	CongestionControlConfig config.CongestionControlConfig
	Telemetry               telemetry.TelemetryService
	EnabledCodecs           []*livekit.Codec
	Logger                  logger.Logger
	SimTracks               map[uint32]SimulcastTrackInfo
}

func newPeerConnection(params TransportParams, onBandwidthEstimator func(estimator cc.BandwidthEstimator)) (*webrtc.PeerConnection, *webrtc.MediaEngine, error) {
	var directionConfig DirectionConfig
	if params.Target == livekit.SignalTarget_PUBLISHER {
		directionConfig = params.Config.Publisher
	} else {
		directionConfig = params.Config.Subscriber
	}
	me, err := createMediaEngine(params.EnabledCodecs, directionConfig)
	if err != nil {
		return nil, nil, err
	}

	se := params.Config.SettingEngine
	se.DisableMediaEngineCopy(true)
	//
	// Disable SRTP replay protection (https://datatracker.ietf.org/doc/html/rfc3711#page-15).
	// Needed due to lack of RTX stream support in Pion.
	//
	// When clients probe for bandwidth, there are several possible approaches
	//   1. Use padding packet (Chrome uses this)
	//   2. Use an older packet (Firefox uses this)
	// Typically, these are sent over the RTX stream and hence SRTP replay protection will not
	// trigger. As Pion does not support RTX, when firefox uses older packet for probing, they
	// trigger the replay protection.
	//
	// That results in two issues
	//   - Firefox bandwidth probing is not successful
	//   - Pion runs out of read buffer capacity - this potentially looks like a Pion issue
	//
	// NOTE: It is not required to disable RTCP replay protection, but doing it to be symmetric.
	//
	se.DisableSRTPReplayProtection(true)
	se.DisableSRTCPReplayProtection(true)
	if !params.ProtocolVersion.SupportsICELite() {
		se.SetLite(false)
	}
	se.SetDTLSRetransmissionInterval(dtlsRetransmissionInterval)
	se.SetICETimeouts(iceDisconnectedTimeout, iceFailedTimeout, iceKeepaliveInterval)

	lf := serverlogger.NewLoggerFactory(logr.Logger(params.Logger))
	if lf != nil {
		se.LoggerFactory = lf
	}

	ir := &interceptor.Registry{}
	if params.Target == livekit.SignalTarget_SUBSCRIBER {
		isSendSideBWE := false
		for _, ext := range directionConfig.RTPHeaderExtension.Video {
			if ext == sdp.TransportCCURI {
				isSendSideBWE = true
				break
			}
		}
		for _, ext := range directionConfig.RTPHeaderExtension.Audio {
			if ext == sdp.TransportCCURI {
				isSendSideBWE = true
				break
			}
		}

		if isSendSideBWE {
			gf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
				return gcc.NewSendSideBWE(
					gcc.SendSideBWEInitialBitrate(1*1000*1000),
					gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
				)
			})
			if err == nil {
				gf.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
					if onBandwidthEstimator != nil {
						onBandwidthEstimator(estimator)
					}
				})
				ir.Add(gf)

				tf, err := twcc.NewHeaderExtensionInterceptor()
				if err == nil {
					ir.Add(tf)
				}
			}
		}
	}
	if len(params.SimTracks) > 0 {
		f, err := NewUnhandleSimulcastInterceptorFactory(UnhandleSimulcastTracks(params.SimTracks))
		if err != nil {
			params.Logger.Errorw("NewUnhandleSimulcastInterceptorFactory failed", err)
		} else {
			ir.Add(f)
		}
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(ir),
	)
	pc, err := api.NewPeerConnection(params.Config.Configuration)
	return pc, me, err
}

func NewPCTransport(params TransportParams) (*PCTransport, error) {
	t := &PCTransport{
		params:             params,
		debouncedNegotiate: debounce.New(negotiationFrequency),
		negotiationState:   negotiationStateNone,
		negotiationPending: make(map[livekit.ParticipantID]bool),
	}
	if params.Target == livekit.SignalTarget_SUBSCRIBER {
		t.streamAllocator = sfu.NewStreamAllocator(sfu.StreamAllocatorParams{
			Config: params.CongestionControlConfig,
			Logger: params.Logger,
		})
		t.streamAllocator.Start()
	}

	if err := t.createPeerConnection(); err != nil {
		return nil, err
	}

	return t, nil
}

func (t *PCTransport) Logger() logger.Logger {
	return t.params.Logger
}

func (t *PCTransport) SetICEConnectedAt(at time.Time) {
	t.lock.Lock()
	t.iceConnectedAt = at
	t.lock.Unlock()
}

func (t *PCTransport) IsShortConnection(at time.Time) (bool, time.Duration) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if t.iceConnectedAt.IsZero() {
		return false, 0
	}

	duration := at.Sub(t.iceConnectedAt)
	return duration < shortConnectionThreshold, duration
}

func (t *PCTransport) GetSelectedPair() (*webrtc.ICECandidatePair, error) {
	sctp := t.pc.SCTP()
	if sctp == nil {
		return nil, errors.New("no SCTP")
	}

	dtlsTransport := sctp.Transport()
	if dtlsTransport == nil {
		return nil, errors.New("no DTLS transport")
	}

	iceTransport := dtlsTransport.ICETransport()
	if iceTransport == nil {
		return nil, errors.New("no ICE transport")
	}

	return iceTransport.GetSelectedCandidatePair()
}

func (t *PCTransport) SetPreferTCP(preferTCP bool) {
	t.lock.Lock()
	t.preferTCP = preferTCP
	t.lock.Unlock()
}

func (t *PCTransport) createPeerConnection() error {
	var bwe cc.BandwidthEstimator
	pc, me, err := newPeerConnection(t.params, func(estimator cc.BandwidthEstimator) {
		bwe = estimator
	})
	if err != nil {
		return err
	}

	t.pc = pc
	t.pc.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
		if state == webrtc.ICEGathererStateComplete {
			go func() {
				t.lock.Lock()
				if t.restartAfterGathering {
					t.params.Logger.Debugw("restarting ICE after ICE gathering")
					if err := t.createAndSendOffer(&webrtc.OfferOptions{ICERestart: true}); err != nil {
						t.params.Logger.Warnw("could not restart ICE", err)
					}
					t.lock.Unlock()
				} else if t.pendingRestartIceOffer != nil {
					t.params.Logger.Debugw("accept remote restart ice offer after ICE gathering")
					offer := t.pendingRestartIceOffer
					t.pendingRestartIceOffer = nil
					t.lock.Unlock()
					if err := t.SetRemoteDescription(*offer); err != nil {
						t.params.Logger.Warnw("could not accept remote restart ice offer", err)
					}
				} else {
					t.lock.Unlock()
				}
			}()
		}
	})

	t.me = me

	if bwe != nil && t.streamAllocator != nil {
		t.streamAllocator.SetBandwidthEstimator(bwe)
	}

	return nil
}

func (t *PCTransport) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if t.pc.RemoteDescription() == nil {
		t.lock.Lock()
		t.pendingCandidates = append(t.pendingCandidates, candidate)
		t.lock.Unlock()
		return nil
	}

	t.params.Logger.Debugw("add candidate ", "candidate", candidate.Candidate)

	return t.pc.AddICECandidate(candidate)
}

func (t *PCTransport) PeerConnection() *webrtc.PeerConnection {
	return t.pc
}

// IsEstablished returns true if the PeerConnection has been established
func (t *PCTransport) IsEstablished() bool {
	return t.pc.ConnectionState() != webrtc.PeerConnectionStateNew
}

func (t *PCTransport) Close() {
	t.lock.Lock()
	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}
	t.lock.Unlock()

	if t.streamAllocator != nil {
		t.streamAllocator.Stop()
	}

	_ = t.pc.Close()
}

func (t *PCTransport) SetRemoteDescription(sd webrtc.SessionDescription) error {
	t.lock.Lock()

	var (
		iceCredential   string
		offerRestartICE bool
	)
	if sd.Type == webrtc.SDPTypeOffer {
		var err error
		iceCredential, offerRestartICE, err = t.isRemoteOfferRestartICE(sd)
		if err != nil {
			t.Logger().Errorw("check remote offer restart ice failed", err)
			t.lock.Unlock()
			return err
		}
	}

	if offerRestartICE && t.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
		t.Logger().Debugw("remote offer restart ice while ice gathering")
		t.pendingRestartIceOffer = &sd
		t.lock.Unlock()
		return nil
	}

	if err := t.pc.SetRemoteDescription(sd); err != nil {
		t.lock.Unlock()
		return err
	}

	if t.currentOfferIceCredential == "" || offerRestartICE {
		t.currentOfferIceCredential = iceCredential
	}

	// negotiated, reset flag
	lastState := t.negotiationState
	t.negotiationState = negotiationStateNone

	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}

	for _, c := range t.pendingCandidates {
		if err := t.pc.AddICECandidate(c); err != nil {
			t.lock.Unlock()
			return err
		}
	}
	t.pendingCandidates = nil

	// only initiate when we are the offerer
	if lastState == negotiationRetry && sd.Type == webrtc.SDPTypeAnswer {
		t.params.Logger.Debugw("re-negotiate after receiving answer")
		if err := t.createAndSendOffer(nil); err != nil {
			t.params.Logger.Errorw("could not negotiate", err)
		}
	}
	onRemoteDescripitonSettled := t.onRemoteDescripitonSettled
	t.lock.Unlock()

	if onRemoteDescripitonSettled != nil {
		return onRemoteDescripitonSettled()
	}
	return nil
}

func (t *PCTransport) isRemoteOfferRestartICE(sd webrtc.SessionDescription) (string, bool, error) {
	parsed, err := sd.Unmarshal()
	if err != nil {
		return "", false, err
	}
	user, pwd, err := extractICECredential(parsed)
	if err != nil {
		return "", false, err
	}

	credential := fmt.Sprintf("%s:%s", user, pwd)
	// ice credential changed, remote offer restart ice
	restartICE := t.currentOfferIceCredential != "" && t.currentOfferIceCredential != credential
	return credential, restartICE, nil
}

// OnOffer is called when the PeerConnection starts negotiation and prepares an offer
func (t *PCTransport) OnOffer(f func(sd webrtc.SessionDescription)) {
	t.onOffer = f
}

func (t *PCTransport) OnRemoteDescripitonSettled(f func() error) {
	t.lock.Lock()
	t.onRemoteDescripitonSettled = f
	t.lock.Unlock()
}

func (t *PCTransport) OnNegotiationFailed(f func()) {
	t.onNegotiationFailed = f
}

func (t *PCTransport) AddNegotiationPending(publisherID livekit.ParticipantID) {
	t.lock.Lock()
	t.negotiationPending[publisherID] = true
	t.lock.Unlock()
}

func (t *PCTransport) Negotiate(force bool) {
	if force {
		t.debouncedNegotiate(func() {
			// no op to cancel pending negotiation
		})
		if err := t.CreateAndSendOffer(nil); err != nil {
			t.params.Logger.Errorw("could not negotiate", err)
		}
	} else {
		t.debouncedNegotiate(func() {
			if err := t.CreateAndSendOffer(nil); err != nil {
				t.params.Logger.Errorw("could not negotiate", err)
			}
		})
	}
}

func (t *PCTransport) IsNegotiationPending(publisherID livekit.ParticipantID) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.negotiationPending[publisherID]
}

func (t *PCTransport) CreateAndSendOffer(options *webrtc.OfferOptions) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.createAndSendOffer(options)
}

// creates and sends offer assuming lock has been acquired
func (t *PCTransport) createAndSendOffer(options *webrtc.OfferOptions) error {
	if t.onOffer == nil {
		return nil
	}
	if t.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return nil
	}

	iceRestart := (options != nil && options.ICERestart) || t.restartAtNextOffer

	// if restart is requested, and we are not ready, then continue afterwards
	if iceRestart {
		if t.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			t.params.Logger.Debugw("restart ICE after gathering")
			t.restartAfterGathering = true
			return nil
		}
		t.params.Logger.Debugw("restarting ICE")
	}

	if iceRestart && t.negotiationState != negotiationStateNone {
		currentSD := t.pc.CurrentRemoteDescription()
		if currentSD == nil {
			// restart without current remote description, sent current local description again to try recover
			offer := t.pc.LocalDescription()
			if offer == nil {
				// it should not happen, log just in case
				t.params.Logger.Warnw("ice restart without local offer", nil)
				return ErrIceRestartWithoutLocalSDP
			} else {
				t.negotiationState = negotiationRetry
				t.restartAtNextOffer = true
				go t.onOffer(*offer)
				return nil
			}
		} else {
			// recover by re-applying the last answer
			t.params.Logger.Infow("recovering from client negotiation state on ICE restart")
			if err := t.pc.SetRemoteDescription(*currentSD); err != nil {
				prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "remote_description").Add(1)
				return err
			}
		}
	} else {
		// when there's an ongoing negotiation, let it finish and not disrupt its state
		if t.negotiationState == negotiationStateClient {
			t.params.Logger.Infow("skipping negotiation, trying again later")
			t.negotiationState = negotiationRetry
			return nil
		} else if t.negotiationState == negotiationRetry {
			// already set to retry, we can safely skip this attempt
			return nil
		}
	}

	ensureICERestart := func(options *webrtc.OfferOptions) *webrtc.OfferOptions {
		if options == nil {
			options = &webrtc.OfferOptions{}
		}
		options.ICERestart = true
		return options
	}

	if t.previousAnswer != nil {
		t.previousAnswer = nil
		options = ensureICERestart(options)
	}

	if t.restartAtNextOffer {
		t.restartAtNextOffer = false
		options = ensureICERestart(options)
	}

	offer, err := t.pc.CreateOffer(options)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "create").Add(1)
		t.params.Logger.Errorw("could not create offer", err)
		return err
	}

	offer = t.filterCandidates(offer)

	err = t.pc.SetLocalDescription(offer)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "local_description").Add(1)
		t.params.Logger.Errorw("could not set local description", err)
		return err
	}

	// indicate waiting for client
	t.negotiationState = negotiationStateClient
	t.restartAfterGathering = false
	t.negotiationPending = make(map[livekit.ParticipantID]bool)

	negotiateVersion := t.negotiateCounter.Inc()
	if t.signalStateCheckTimer != nil {
		t.signalStateCheckTimer.Stop()
		t.signalStateCheckTimer = nil
	}
	t.signalStateCheckTimer = time.AfterFunc(negotiationFailedTimout, func() {
		t.lock.RLock()
		failed := t.negotiationState != negotiationStateNone
		t.lock.RUnlock()
		if t.negotiateCounter.Load() == negotiateVersion && failed {
			if t.onNegotiationFailed != nil {
				t.onNegotiationFailed()
			}
		}
	})

	go t.onOffer(offer)
	return nil
}

func (t *PCTransport) preparePC(previousAnswer webrtc.SessionDescription) error {
	// sticky data channel to first m-lines, if someday we don't send sdp without media streams to
	// client's subscribe pc after joining, should change this step
	parsed, err := previousAnswer.Unmarshal()
	if err != nil {
		return err
	}
	fp, fpHahs, err := extractFingerprint(parsed)
	if err != nil {
		return err
	}

	offer, err := t.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	t.pc.SetLocalDescription(offer)

	//
	// Simulate client side peer connection and set DTLS role from previous answer.
	// Role needs to be set properly (one side needs to be server and the other side
	// needs to be the client) for DTLS connection to form properly. As this is
	// trying to replicate previous setup, read from previous answer and use that role.
	//
	se := webrtc.SettingEngine{}
	se.SetAnsweringDTLSRole(extractDTLSRole(parsed))
	api := webrtc.NewAPI(
		webrtc.WithSettingEngine(se),
		webrtc.WithMediaEngine(t.me),
	)
	pc2, err := api.NewPeerConnection(webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		return err
	}
	defer pc2.Close()

	pc2.SetRemoteDescription(offer)
	ans, err := pc2.CreateAnswer(nil)
	if err != nil {
		return err
	}

	// replace client's fingerprint into dump pc's answer, for pion's dtls process, it will
	// keep the fingerprint at first call of SetRemoteDescription, if dumb pc and client pc use
	// different fingerprint, that will cause pion denied dtls data after handshake with client
	// complete (can't pass fingerprint change).
	// in this step, we don't established connection with dump pc(no candidate swap), just use
	// sdp negotiation to sticky data channel and keep client's fingerprint
	parsedAns, _ := ans.Unmarshal()
	fpLine := fpHahs + " " + fp
	replaceFP := func(attrs []sdp.Attribute, fpLine string) {
		for k := range attrs {
			if attrs[k].Key == "fingerprint" {
				attrs[k].Value = fpLine
			}
		}
	}
	replaceFP(parsedAns.Attributes, fpLine)
	for _, m := range parsedAns.MediaDescriptions {
		replaceFP(m.Attributes, fpLine)
	}
	bytes, err := parsedAns.Marshal()
	if err != nil {
		return err
	}
	ans.SDP = string(bytes)

	return t.pc.SetRemoteDescription(ans)
}

func (t *PCTransport) initPCWithPreviousAnswer(previousAnswer webrtc.SessionDescription) error {
	parsed, err := previousAnswer.Unmarshal()
	if err != nil {
		return err
	}
	for _, m := range parsed.MediaDescriptions {
		var codecType webrtc.RTPCodecType
		switch m.MediaName.Media {
		case "video":
			codecType = webrtc.RTPCodecTypeVideo
		case "audio":
			codecType = webrtc.RTPCodecTypeAudio
		case "application":
			// for pion generate unmatched sdp, it always appends data channel to last m-lines,
			// that not consistent with our previous answer that data channel might at middle-line
			// because sdp can negotiate multi times before migration.(it will sticky to the last m-line atfirst negotiate)
			// so use a dumb pc to negotiate sdp to fixed the datachannel's mid at same position with previous answer
			if err := t.preparePC(previousAnswer); err != nil {
				t.params.Logger.Errorw("prepare pc for migration failed", err)
				return err
			}
			continue
		default:
			continue
		}
		tr, err := t.pc.AddTransceiverFromKind(codecType, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		if err != nil {
			return err
		}
		tr.Stop()
		mid := getMidValue(m)
		if mid == "" {
			return errors.New("mid value not found")
		}
		tr.SetMid(mid)
	}
	return nil
}

func (t *PCTransport) OnStreamStateChange(f func(update *sfu.StreamStateUpdate) error) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.OnStreamStateChange(f)
}

func (t *PCTransport) AddTrack(subTrack types.SubscribedTrack) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.AddTrack(subTrack.DownTrack(), sfu.AddTrackParams{
		Source:      subTrack.MediaTrack().Source(),
		IsSimulcast: subTrack.MediaTrack().IsSimulcast(),
		PublisherID: subTrack.MediaTrack().PublisherID(),
	})
}

func (t *PCTransport) RemoveTrack(subTrack types.SubscribedTrack) {
	if t.streamAllocator == nil {
		return
	}

	t.streamAllocator.RemoveTrack(subTrack.DownTrack())
}

func (t *PCTransport) SetPreviousAnswer(answer *webrtc.SessionDescription) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.pc.RemoteDescription() == nil && t.previousAnswer == nil {
		t.previousAnswer = answer
		if err := t.initPCWithPreviousAnswer(*t.previousAnswer); err != nil {
			t.params.Logger.Errorw("initPCWithPreviousAnswer failed", err)
		}
	}
}

func (t *PCTransport) FilterCandidates(sd webrtc.SessionDescription) webrtc.SessionDescription {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.filterCandidates(sd)
}

func (t *PCTransport) filterCandidates(sd webrtc.SessionDescription) webrtc.SessionDescription {
	parsed, err := sd.Unmarshal()
	if err != nil {
		t.params.Logger.Errorw("could not unmarshal SDP to filter candidates", err)
		return sd
	}

	filterAttributes := func(attrs []sdp.Attribute) []sdp.Attribute {
		filteredAttrs := make([]sdp.Attribute, 0, len(attrs))
		for _, a := range attrs {
			if a.Key == sdp.AttrKeyCandidate {
				if t.preferTCP {
					if strings.Contains(a.Value, "tcp") {
						filteredAttrs = append(filteredAttrs, a)
					}
				} else {
					filteredAttrs = append(filteredAttrs, a)
				}
			} else {
				filteredAttrs = append(filteredAttrs, a)
			}
		}

		return filteredAttrs
	}

	parsed.Attributes = filterAttributes(parsed.Attributes)
	for _, m := range parsed.MediaDescriptions {
		m.Attributes = filterAttributes(m.Attributes)
	}

	bytes, err := parsed.Marshal()
	if err != nil {
		t.params.Logger.Errorw("could not marshal SDP to filter candidates", err)
		return sd
	}
	sd.SDP = string(bytes)
	return sd
}

// ---------------------------------------------

func getMidValue(media *sdp.MediaDescription) string {
	for _, attr := range media.Attributes {
		if attr.Key == sdp.AttrKeyMID {
			return attr.Value
		}
	}
	return ""
}

func extractFingerprint(desc *sdp.SessionDescription) (string, string, error) {
	fingerprints := make([]string, 0)

	if fingerprint, haveFingerprint := desc.Attribute("fingerprint"); haveFingerprint {
		fingerprints = append(fingerprints, fingerprint)
	}

	for _, m := range desc.MediaDescriptions {
		if fingerprint, haveFingerprint := m.Attribute("fingerprint"); haveFingerprint {
			fingerprints = append(fingerprints, fingerprint)
		}
	}

	if len(fingerprints) < 1 {
		return "", "", webrtc.ErrSessionDescriptionNoFingerprint
	}

	for _, m := range fingerprints {
		if m != fingerprints[0] {
			return "", "", webrtc.ErrSessionDescriptionConflictingFingerprints
		}
	}

	parts := strings.Split(fingerprints[0], " ")
	if len(parts) != 2 {
		return "", "", webrtc.ErrSessionDescriptionInvalidFingerprint
	}
	return parts[1], parts[0], nil
}

func extractDTLSRole(desc *sdp.SessionDescription) webrtc.DTLSRole {
	for _, md := range desc.MediaDescriptions {
		setup, ok := md.Attribute(sdp.AttrKeyConnectionSetup)
		if !ok {
			continue
		}

		if setup == sdp.ConnectionRoleActive.String() {
			return webrtc.DTLSRoleClient
		}

		if setup == sdp.ConnectionRolePassive.String() {
			return webrtc.DTLSRoleServer
		}
	}

	//
	// If 'setup' attribute is not available, use client role
	// as that is the default behaviour of answerers
	//
	// There seems to be some differences in how role is decided.
	// libwebrtc (Chrome) code - (https://source.chromium.org/chromium/chromium/src/+/main:third_party/webrtc/pc/jsep_transport.cc;l=592;drc=369fb686729e7eb20d2bd09717cec14269a399d7)
	// does not mention anything about ICE role when determining
	// DTLS Role.
	//
	// But, ORTC has this - https://github.com/w3c/ortc/issues/167#issuecomment-69409953
	// and pion/webrtc follows that (https://github.com/pion/webrtc/blob/e071a4eded1efd5d9b401bcfc4efacb3a2a5a53c/dtlstransport.go#L269)
	//
	// So if remote is ice-lite, pion will use DTLSRoleServer when answering
	// while browsers pick DTLSRoleClient.
	//
	return webrtc.DTLSRoleClient
}

func extractICECredential(desc *sdp.SessionDescription) (string, string, error) {
	remotePwds := []string{}
	remoteUfrags := []string{}

	if ufrag, haveUfrag := desc.Attribute("ice-ufrag"); haveUfrag {
		remoteUfrags = append(remoteUfrags, ufrag)
	}
	if pwd, havePwd := desc.Attribute("ice-pwd"); havePwd {
		remotePwds = append(remotePwds, pwd)
	}

	for _, m := range desc.MediaDescriptions {
		if ufrag, haveUfrag := m.Attribute("ice-ufrag"); haveUfrag {
			remoteUfrags = append(remoteUfrags, ufrag)
		}
		if pwd, havePwd := m.Attribute("ice-pwd"); havePwd {
			remotePwds = append(remotePwds, pwd)
		}
	}

	if len(remoteUfrags) == 0 {
		return "", "", webrtc.ErrSessionDescriptionMissingIceUfrag
	} else if len(remotePwds) == 0 {
		return "", "", webrtc.ErrSessionDescriptionMissingIcePwd
	}

	for _, m := range remoteUfrags {
		if m != remoteUfrags[0] {
			return "", "", webrtc.ErrSessionDescriptionConflictingIceUfrag
		}
	}

	for _, m := range remotePwds {
		if m != remotePwds[0] {
			return "", "", webrtc.ErrSessionDescriptionConflictingIcePwd
		}
	}

	return remoteUfrags[0], remotePwds[0], nil
}
