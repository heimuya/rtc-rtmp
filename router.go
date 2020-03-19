package rtcrtmp

import (
	"bytes"
	"fmt"
	"github.com/notedit/rtc-rtmp/transformer"
	"github.com/notedit/rtmp-lib"
	"github.com/notedit/rtmp-lib/aac"
	"github.com/notedit/rtmp-lib/av"
	"github.com/notedit/rtmp-lib/h264"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
	uuid "github.com/satori/go.uuid"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"time"
)

var NALUHeader = []byte{0, 0, 0, 1}

type RTCRouter struct {
	streamID   string
	streamURL  string
	streams    []av.CodecData
	videoCodec h264.CodecData
	audioCodec aac.CodecData

	conn      *rtmp.Conn

	transform *transformer.Transformer
	lastVideoTime time.Duration
	lastAudioTime time.Duration

	videoPacketizer rtp.Packetizer
	audioPacketizer rtp.Packetizer

	outTransports map[string]*RTCTransport
	sync.RWMutex
}

func NewRTCRouter(streamURL string) (router *RTCRouter,err error) {

	var u *url.URL
	u, err = url.Parse(streamURL)
	if err != nil {
		return
	}

	streaminfo := strings.Split(u.Path, "/")
	if len(streaminfo) <= 2 {
		err = fmt.Errorf("rtmp url does not match")
		return
	}
	streamID := streaminfo[len(streaminfo)-1]

	conn, err := rtmp.DialTimeout(streamURL,3 *time.Second)

	if err != nil {
		return
	}

	videoCodec := webrtc.NewRTPH264Codec(webrtc.DefaultPayloadTypeH264, 90000)
	audioCodec := webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000)

	videoPacketizer := rtp.NewPacketizer(
		1200,
		videoCodec.PayloadType,
		rand.Uint32(),
		videoCodec.Payloader,
		rtp.NewRandomSequencer(),
		videoCodec.ClockRate,
		)

	audioPacketizer := rtp.NewPacketizer(
		1200,
		audioCodec.PayloadType,
		rand.Uint32(),
		audioCodec.Payloader,
		rtp.NewRandomSequencer(),
		audioCodec.ClockRate,
		)

	router = &RTCRouter{}
	router.streamURL = streamURL
	router.streamID = streamID
	router.conn = conn
	router.videoPacketizer = videoPacketizer
	router.audioPacketizer = audioPacketizer

	return
}

func (self *RTCRouter) CreateSubscriber() (*RTCTransport,error) {

	id := uuid.NewV4().String()
	transport,err := NewRTCTransport(id)

	if err != nil {
		return nil,err
	}

	self.Lock()
	self.outTransports[id] = transport
	self.Unlock()

	return transport,nil
}

func (self *RTCRouter) StopSubscriber(transport *RTCTransport) {

	self.Lock()
	delete(self.outTransports,transport.ID())
	self.Unlock()
}


func (self *RTCRouter) readPacket() {

	var err error
	self.streams, err = self.conn.Streams()
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, stream := range self.streams {
		if stream.Type() == av.H264 {
			self.videoCodec = stream.(h264.CodecData)
		}
		if stream.Type() == av.AAC {
			self.audioCodec = stream.(aac.CodecData)
			self.transform.SetInSampleRate(self.audioCodec.SampleRate())
			self.transform.SetInChannelLayout(self.audioCodec.ChannelLayout())
			self.transform.SetInSampleFormat(self.audioCodec.SampleFormat())
			self.transform.SetOutChannelLayout(av.CH_STEREO)
			self.transform.SetOutSampleRate(48000)
			self.transform.SetOutSampleFormat(av.S16)
			self.transform.Setup()
		}
	}

	for {
		packet, err := self.conn.ReadPacket()
		if err != nil {
			break
		}

		stream := self.streams[packet.Idx]

		if stream.Type().IsVideo() {
			var samples uint32
			if self.lastVideoTime == 0 {
				samples = 0
			} else {
				samples = uint32(uint64(packet.Time-self.lastVideoTime) * 90000 / 1000000000)
			}

			var b bytes.Buffer
			// TODO  may check the sps and ppt packet
			if packet.IsKeyFrame {
				b.Write(NALUHeader)
				b.Write(self.videoCodec.SPS())
				b.Write(NALUHeader)
				b.Write(self.videoCodec.PPS())
			}

			if packet.Data[0] == 0x00 && packet.Data[1] == 0x00 && packet.Data[2] == 0x00 && packet.Data[3] == 0x01 {
				b.Write(packet.Data)
			} else {
				nalus, _ := h264.SplitNALUs(packet.Data)
				for _, nalu := range nalus {
					b.Write(naluHeader)
					b.Write(nalu)
				}
			}

			packets := self.videoPacketizer.Packetize(b.Bytes(), samples)
			self.writePackets(packets)
			self.lastVideoTime = packet.Time

		} else if stream.Type() == av.AAC {

			pkts,err := self.transform.Do(packet)
			if err != nil {
				fmt.Println("transform error",err)
				continue
			}

			for _,pkt := range pkts {
				packets := self.audioPacketizer.Packetize(pkt.Data, 960)
				self.writePackets(packets)
				self.lastAudioTime = pkt.Time
			}
		}
	}
}

func (self *RTCRouter) writePackets(pkts []*rtp.Packet)  {
	self.RLock()
	defer self.RUnlock()

	for _,pkt := range pkts {
		for _,transport := range self.outTransports {
			transport.WriteRTP(pkt)
		}
	}
}

func (self *RTCRouter) Stop() error {
	return nil
}



