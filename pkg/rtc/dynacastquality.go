package rtc

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	initialQualityUpdateWait = 10 * time.Second
)

type DynacastQualityParams struct {
	MimeType           string
	DynacastPauseDelay time.Duration
	Logger             logger.Logger
}

// DynacastQuality manages max subscribed quality of a single receiver of a media track
type DynacastQuality struct {
	params DynacastQualityParams

	// quality level enable/disable
	lock                     sync.RWMutex
	initialized              bool
	maxSubscriberQuality     map[livekit.ParticipantID]livekit.VideoQuality
	maxSubscriberNodeQuality map[livekit.NodeID]livekit.VideoQuality
	maxSubscribedQuality     livekit.VideoQuality
	maxQualityTimer          *time.Timer

	onSubscribedMaxQualityChange func(maxSubscribedQuality livekit.VideoQuality)
}

func NewDynacastQuality(params DynacastQualityParams) *DynacastQuality {
	return &DynacastQuality{
		params:                   params,
		maxSubscriberQuality:     make(map[livekit.ParticipantID]livekit.VideoQuality),
		maxSubscriberNodeQuality: make(map[livekit.NodeID]livekit.VideoQuality),
	}
}

func (d *DynacastQuality) Start() {
	d.reset()
}

func (d *DynacastQuality) Restart() {
	d.reset()
}

func (d *DynacastQuality) Stop() {
	d.stopMaxQualityTimer()
}

func (d *DynacastQuality) OnSubscribedMaxQualityChange(f func(maxSubscribedQuality livekit.VideoQuality)) {
	d.onSubscribedMaxQualityChange = f
}

func (d *DynacastQuality) MimeType() string {
	return d.params.MimeType
}

func (d *DynacastQuality) MaxSubscribedQuality() livekit.VideoQuality {
	d.lock.RLock()
	defer d.lock.RUnlock()

	return d.maxSubscribedQuality
}

func (d *DynacastQuality) NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, quality livekit.VideoQuality) {
	d.lock.Lock()
	if quality == livekit.VideoQuality_OFF {
		delete(d.maxSubscriberQuality, subscriberID)
	} else {
		d.maxSubscriberQuality[subscriberID] = quality
	}
	d.lock.Unlock()

	d.updateQualityChange()
}

func (d *DynacastQuality) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, quality livekit.VideoQuality) {
	d.lock.Lock()
	if quality == livekit.VideoQuality_OFF {
		delete(d.maxSubscriberNodeQuality, nodeID)
	} else {
		d.maxSubscriberNodeQuality[nodeID] = quality
	}
	d.lock.Unlock()

	d.updateQualityChange()
}

func (d *DynacastQuality) reset() {
	d.lock.Lock()
	d.initialized = false
	d.maxSubscribedQuality = livekit.VideoQuality_HIGH
	d.lock.Unlock()

	d.startMaxQualityTimer()
}

func (d *DynacastQuality) updateQualityChange() {
	d.lock.Lock()
	maxSubscribedQuality := livekit.VideoQuality_OFF
	for _, subQuality := range d.maxSubscriberQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || (subQuality != livekit.VideoQuality_OFF && subQuality > maxSubscribedQuality) {
			maxSubscribedQuality = subQuality
		}
	}
	for _, nodeQuality := range d.maxSubscriberNodeQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || (nodeQuality != livekit.VideoQuality_OFF && nodeQuality > maxSubscribedQuality) {
			maxSubscribedQuality = nodeQuality
		}
	}

	if maxSubscribedQuality == d.maxSubscribedQuality && d.initialized {
		d.lock.Unlock()
		return
	}

	d.initialized = true
	d.maxSubscribedQuality = maxSubscribedQuality
	d.params.Logger.Infow("notifying quality change",
		"mime", d.params.MimeType,
		"maxSubscriberQuality", d.maxSubscriberQuality,
		"maxSubscriberNodeQuality", d.maxSubscriberNodeQuality,
		"maxSubscribedQuality", d.maxSubscribedQuality,
	)
	onSubscribedMaxQualityChange := d.onSubscribedMaxQualityChange
	d.lock.Unlock()

	if onSubscribedMaxQualityChange != nil {
		onSubscribedMaxQualityChange(maxSubscribedQuality)
	}
}

func (d *DynacastQuality) startMaxQualityTimer() {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.maxQualityTimer != nil {
		d.maxQualityTimer.Stop()
		d.maxQualityTimer = nil
	}

	d.maxQualityTimer = time.AfterFunc(initialQualityUpdateWait, func() {
		d.stopMaxQualityTimer()
		d.updateQualityChange()
	})
}

func (d *DynacastQuality) stopMaxQualityTimer() {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.maxQualityTimer != nil {
		d.maxQualityTimer.Stop()
		d.maxQualityTimer = nil
	}
}
