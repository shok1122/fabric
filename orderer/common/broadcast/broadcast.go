/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package broadcast

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gogo/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/ledger/rwset"
	"github.com/hyperledger/fabric-protos-go/ledger/rwset/kvrwset"
	ab "github.com/hyperledger/fabric-protos-go/orderer"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/orderer/common/msgprocessor"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
)

var logger = flogging.MustGetLogger("orderer.common.broadcast")

//go:generate counterfeiter -o mock/channel_support_registrar.go --fake-name ChannelSupportRegistrar . ChannelSupportRegistrar

// ChannelSupportRegistrar provides a way for the Handler to look up the Support for a channel
type ChannelSupportRegistrar interface {
	// BroadcastChannelSupport returns the message channel header, whether the message is a config update
	// and the channel resources for a message or an error if the message is not a message which can
	// be processed directly (like CONFIG and ORDERER_TRANSACTION messages)
	BroadcastChannelSupport(msg *cb.Envelope) (*cb.ChannelHeader, bool, ChannelSupport, error)
}

//go:generate counterfeiter -o mock/channel_support.go --fake-name ChannelSupport . ChannelSupport

// ChannelSupport provides the backing resources needed to support broadcast on a channel
type ChannelSupport interface {
	msgprocessor.Processor
	Consenter
}

// Consenter provides methods to send messages through consensus
type Consenter interface {
	// Order accepts a message or returns an error indicating the cause of failure
	// It ultimately passes through to the consensus.Chain interface
	Order(env *cb.Envelope, configSeq uint64) error

	// Configure accepts a reconfiguration or returns an error indicating the cause of failure
	// It ultimately passes through to the consensus.Chain interface
	Configure(config *cb.Envelope, configSeq uint64) error

	// WaitReady blocks waiting for consenter to be ready for accepting new messages.
	// This is useful when consenter needs to temporarily block ingress messages so
	// that in-flight messages can be consumed. It could return error if consenter is
	// in erroneous states. If this blocking behavior is not desired, consenter could
	// simply return nil.
	WaitReady() error
}

// Handler is designed to handle connections from Broadcast AB gRPC service
type Handler struct {
	SupportRegistrar ChannelSupportRegistrar
	Metrics          *Metrics
}

// Handle reads requests from a Broadcast stream, processes them, and returns the responses to the stream
func (bh *Handler) Handle(srv ab.AtomicBroadcast_BroadcastServer) error {
	addr := util.ExtractRemoteAddress(srv.Context())
	logger.Debugf("Starting new broadcast loop for %s", addr)
	for {
		msg, err := srv.Recv()
		if err == io.EOF {
			logger.Debugf("Received EOF from %s, hangup", addr)
			return nil
		}
		if err != nil {
			logger.Warningf("Error reading from %s: %s", addr, err)
			return err
		}

		resp := bh.ProcessMessage(msg, addr)
		err = srv.Send(resp)
		if resp.Status != cb.Status_SUCCESS {
			return err
		}

		if err != nil {
			logger.Warningf("Error sending to %s: %s", addr, err)
			return err
		}
	}

}

type MetricsTracker struct {
	ValidateStartTime time.Time
	EnqueueStartTime  time.Time
	ValidateDuration  time.Duration
	ChannelID         string
	TxType            string
	Metrics           *Metrics
}

func (mt *MetricsTracker) Record(resp *ab.BroadcastResponse) {
	labels := []string{
		"status", resp.Status.String(),
		"channel", mt.ChannelID,
		"type", mt.TxType,
	}

	if mt.ValidateDuration == 0 {
		mt.EndValidate()
	}
	mt.Metrics.ValidateDuration.With(labels...).Observe(mt.ValidateDuration.Seconds())

	if mt.EnqueueStartTime != (time.Time{}) {
		enqueueDuration := time.Since(mt.EnqueueStartTime)
		mt.Metrics.EnqueueDuration.With(labels...).Observe(enqueueDuration.Seconds())
	}

	mt.Metrics.ProcessedCount.With(labels...).Add(1)
}

func (mt *MetricsTracker) BeginValidate() {
	mt.ValidateStartTime = time.Now()
}

func (mt *MetricsTracker) EndValidate() {
	mt.ValidateDuration = time.Since(mt.ValidateStartTime)
}

func (mt *MetricsTracker) BeginEnqueue() {
	mt.EnqueueStartTime = time.Now()
}

func GetHeaderString(a_psHeader *cb.Header, a_strIndex string) string {
	sChHeader, _ := protoutil.UnmarshalChannelHeader(a_psHeader.GetChannelHeader())
	sSigHeader, _ := protoutil.UnmarshalSignatureHeader(a_psHeader.GetSignatureHeader())
	sCreator, _ := protoutil.UnmarshalSerializedIdentity(sSigHeader.GetCreator())

	strChHeader, _ := json.Marshal(sChHeader)

	return fmt.Sprintf(""+
		"%[1]schannel-header: %[2]s\n"+
		"%[1]ssignature-header: \n"+
		"%[1]s  creator: \n"+
		"%[1]s    mspid: %[3]s\n"+
		"%[1]s    id_bytes: %[4]s\n"+
		"%[1]s  nonce: %[5]X",
		a_strIndex,
		strChHeader,
		sCreator.Mspid,
		sCreator.IdBytes,
		sSigHeader.Nonce)
}

func GetTxReadWriteSetString(a_psTxReadWriteSet *rwset.TxReadWriteSet, a_strIndex string) string {
	s := fmt.Sprintf("%sdata-model: %s\n",
		a_strIndex,
		rwset.TxReadWriteSet_DataModel_name[int32(a_psTxReadWriteSet.DataModel)])
	for i1, v1 := range a_psTxReadWriteSet.NsRwset {
		psKVRWSet := &kvrwset.KVRWSet{}
		_ = proto.Unmarshal(v1.GetRwset(), psKVRWSet)
		strKVReads := ""
		for i2, v2 := range psKVRWSet.Reads {
			strKVReads += fmt.Sprintf("%s      [%d] key:%s, ver:%#v\n", a_strIndex, i2, v2.Key, v2.Version)
		}
		strKVWrites := ""
		for i2, v2 := range psKVRWSet.Writes {
			strKVWrites += fmt.Sprintf("%s      [%d] key:%s, is-delete:%t, value:%s\n", a_strIndex, i2, v2.Key, v2.IsDelete, string(v2.Value))
		}
		strCollectionHashedRwset := ""
		for _, v2 := range v1.CollectionHashedRwset {
			psHashedRWSet := &kvrwset.HashedRWSet{}
			_ = proto.Unmarshal(v2.HashedRwset, psHashedRWSet)
			strHashedReads := ""
			for i3, v3 := range psHashedRWSet.HashedReads {
				strHashedReads += fmt.Sprintf("%s            [%d] key:%X, ver:%#v\n", a_strIndex, i3, v3.KeyHash, v3.Version)
			}
			strHashedWrites := ""
			for i3, v3 := range psHashedRWSet.HashedWrites {
				strHashedWrites += fmt.Sprintf("%s            [%d] key:%X, is-delete:%t, value:%#v\n", a_strIndex, i3, v3.KeyHash, v3.IsDelete, v3.ValueHash)
			}
			strMetadataWrites := ""
			for i3, v3 := range psHashedRWSet.MetadataWrites {
				strMetadataWrites += fmt.Sprintf("%s            [%d] key:%X, entries:%#v\n", a_strIndex, i3, v3.KeyHash, v3.Entries)
			}
			strCollectionHashedRwset += fmt.Sprintf(""+
				"%[1]s      - collection-name: %[2]s\n"+
				"%[1]s        hashed-rwset: \n"+
				"%[1]s          hashed-reads: \n"+
				"%[3]s \n"+
				"%[1]s          hashed-writes: \n"+
				"%[4]s \n"+
				"%[1]s          metadata-writes: \n"+
				"%[5]s \n"+
				"%[1]s        private-rwset-hash: %[6]X\n",
				a_strIndex,
				v2.CollectionName,
				strHashedReads,
				strHashedWrites,
				strMetadataWrites,
				v2.PvtRwsetHash)
		}
		s += fmt.Sprintf(""+
			"%[1]snsrwset[%[2]d]: \n"+
			"%[1]s  namespace: %[3]s\n"+
			"%[1]s  rwset: \n"+
			"%[1]s    reads: \n"+
			"%[4]s\n"+
			"%[1]s    writes: \n"+
			"%[5]s\n"+
			"%[1]s    collection-hashed-rwset: \n"+
			"%[6]s",
			a_strIndex,
			i1,
			v1.Namespace,
			strKVReads,
			strKVWrites,
			strCollectionHashedRwset)
	}
	return s
}

func GetTransactionActionString(a_psActions []*peer.TransactionAction, a_strIndex string) string {

	s := ""
	for i, v := range a_psActions {
		psHeader, _ := protoutil.UnmarshalHeader(v.Header)
		strHeader := GetHeaderString(psHeader, fmt.Sprintf("%s    ", a_strIndex))
		psPayload, _ := protoutil.UnmarshalChaincodeActionPayload(v.Payload)
		psEndorsedAction := psPayload.GetAction()
		psProposalResponsePayload, _ := protoutil.UnmarshalProposalResponsePayload(psEndorsedAction.ProposalResponsePayload)
		psChaincodeAction, _ := protoutil.UnmarshalChaincodeAction(psProposalResponsePayload.GetExtension())
		psTxReadWriteSet := &rwset.TxReadWriteSet{}
		_ = proto.Unmarshal(psChaincodeAction.GetResults(), psTxReadWriteSet)
		strTxReadWriteSet := GetTxReadWriteSetString(psTxReadWriteSet, fmt.Sprintf("%s          ", a_strIndex))
		psEvents, _ := protoutil.UnmarshalChaincodeEvents(psChaincodeAction.GetEvents())
		psResponse := psChaincodeAction.Response
		s += fmt.Sprintf(""+
			"%[1]saction[%[2]d]: \n"+
			"%[1]s  header: \n"+
			"%[3]s\n"+
			"%[1]s  payload: \n"+
			"%[1]s    action: \n"+
			"%[1]s      proposal-hash: %[4]X\n"+
			"%[1]s      extension: \n"+
			"%[1]s        results: \n"+
			"%[5]s\n"+
			"%[1]s        events: %[6]s\n"+
			"%[1]s        response: %[7]s",
			a_strIndex,
			i,
			strHeader,
			psProposalResponsePayload.GetProposalHash(),
			strTxReadWriteSet,
			psEvents.String(),
			psResponse.String())
	}
	return s
}

func GetPayloadString(msg *cb.Envelope) string {

	payload, _ := protoutil.UnmarshalPayload(msg.GetPayload())

	psHeader := payload.GetHeader()
	strHeader := GetHeaderString(psHeader, "  ")

	bData := payload.GetData()
	sTransaction, _ := protoutil.UnmarshalTransaction(bData)
	sActions := sTransaction.GetActions()

	strAction := GetTransactionActionString(sActions, "    ")

	s := fmt.Sprintf("\n"+
		"header: \n"+
		"%s\n"+
		"data: \n"+
		"  input: \n"+
		"%s\n",
		strHeader,
		strAction)

	return s
}

// ProcessMessage validates and enqueues a single message
func (bh *Handler) ProcessMessage(msg *cb.Envelope, addr string) (resp *ab.BroadcastResponse) {
	tracker := &MetricsTracker{
		ChannelID: "unknown",
		TxType:    "unknown",
		Metrics:   bh.Metrics,
	}
	defer func() {
		// This looks a little unnecessary, but if done directly as
		// a defer, resp gets the (always nil) current state of resp
		// and not the return value
		tracker.Record(resp)
	}()
	tracker.BeginValidate()

	chdr, isConfig, processor, err := bh.SupportRegistrar.BroadcastChannelSupport(msg) // goto orderer/common/multichannel/registrar.go:425
	logger.Debugf("!!! [channel: %s] processor: %#v", chdr.ChannelId, processor)

	if chdr != nil {
		tracker.ChannelID = chdr.ChannelId
		tracker.TxType = cb.HeaderType(chdr.Type).String()
	}
	if err != nil {
		logger.Warningf("[channel: %s] Could not get message processor for serving %s: %s", tracker.ChannelID, addr, err)
		return &ab.BroadcastResponse{Status: cb.Status_BAD_REQUEST, Info: err.Error()}
	}

	if !isConfig {
		logger.Debugf("[channel: %s] Broadcast is processing normal message from %s with txid '%s' of type %s", chdr.ChannelId, addr, chdr.TxId, cb.HeaderType_name[chdr.Type])

		configSeq, err := processor.ProcessNormalMsg(msg)
		if err != nil {
			logger.Warningf("[channel: %s] Rejecting broadcast of normal message from %s because of error: %s", chdr.ChannelId, addr, err)
			return &ab.BroadcastResponse{Status: ClassifyError(err), Info: err.Error()}
		}
		tracker.EndValidate()

		tracker.BeginEnqueue()
		if err = processor.WaitReady(); err != nil {
			logger.Warningf("[channel: %s] Rejecting broadcast of message from %s with SERVICE_UNAVAILABLE: rejected by Consenter: %s", chdr.ChannelId, addr, err)
			return &ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE, Info: err.Error()}
		}

		err = processor.Order(msg, configSeq)
		if err != nil {
			logger.Warningf("[channel: %s] Rejecting broadcast of normal message from %s with SERVICE_UNAVAILABLE: rejected by Order: %s", chdr.ChannelId, addr, err)
			return &ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE, Info: err.Error()}
		}

		logger.Debugf("!!! [channel: %s] msg: %s", chdr.ChannelId, GetPayloadString(msg))

	} else { // isConfig
		logger.Debugf("[channel: %s] Broadcast is processing config update message from %s", chdr.ChannelId, addr)

		config, configSeq, err := processor.ProcessConfigUpdateMsg(msg)
		if err != nil {
			logger.Warningf("[channel: %s] Rejecting broadcast of config message from %s because of error: %s", chdr.ChannelId, addr, err)
			return &ab.BroadcastResponse{Status: ClassifyError(err), Info: err.Error()}
		}
		tracker.EndValidate()

		tracker.BeginEnqueue()
		if err = processor.WaitReady(); err != nil {
			logger.Warningf("[channel: %s] Rejecting broadcast of message from %s with SERVICE_UNAVAILABLE: rejected by Consenter: %s", chdr.ChannelId, addr, err)
			return &ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE, Info: err.Error()}
		}

		err = processor.Configure(config, configSeq)
		if err != nil {
			logger.Warningf("[channel: %s] Rejecting broadcast of config message from %s with SERVICE_UNAVAILABLE: rejected by Configure: %s", chdr.ChannelId, addr, err)
			return &ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE, Info: err.Error()}
		}
	}

	logger.Debugf("[channel: %s] Broadcast has successfully enqueued message of type %s from %s", chdr.ChannelId, cb.HeaderType_name[chdr.Type], addr)

	return &ab.BroadcastResponse{Status: cb.Status_SUCCESS}
}

// ClassifyError converts an error type into a status code.
func ClassifyError(err error) cb.Status {
	switch errors.Cause(err) {
	case msgprocessor.ErrChannelDoesNotExist:
		return cb.Status_NOT_FOUND
	case msgprocessor.ErrPermissionDenied:
		return cb.Status_FORBIDDEN
	case msgprocessor.ErrMaintenanceMode:
		return cb.Status_SERVICE_UNAVAILABLE
	default:
		return cb.Status_BAD_REQUEST
	}
}
