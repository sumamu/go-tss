package eddsa

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/HyperCore-Team/go-tss/keygen"

	bcrypto "github.com/HyperCore-Team/tss-lib/crypto"
	bkg "github.com/HyperCore-Team/tss-lib/eddsa/keygen"
	btss "github.com/HyperCore-Team/tss-lib/tss"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	tcrypto "github.com/tendermint/tendermint/crypto"

	"github.com/HyperCore-Team/go-tss/blame"
	"github.com/HyperCore-Team/go-tss/common"
	"github.com/HyperCore-Team/go-tss/conversion"
	"github.com/HyperCore-Team/go-tss/messages"
	"github.com/HyperCore-Team/go-tss/p2p"
	"github.com/HyperCore-Team/go-tss/storage"
)

type EDDSAKeyGen struct {
	logger          zerolog.Logger
	localNodePubKey string
	tssCommonStruct *common.TssCommon
	stopChan        chan struct{} // channel to indicate whether we should stop
	localParty      *btss.PartyID
	stateManager    storage.LocalStateManager
	commStopChan    chan struct{}
	p2pComm         *p2p.Communication
}

func NewTssKeyGen(localP2PID string,
	conf common.TssConfig,
	localNodePubKey string,
	broadcastChan chan *messages.BroadcastMsgChan,
	stopChan chan struct{},
	msgID string,
	stateManager storage.LocalStateManager,
	privateKey tcrypto.PrivKey,
	p2pComm *p2p.Communication) *EDDSAKeyGen {
	return &EDDSAKeyGen{
		logger: log.With().
			Str("module", "keygen").
			Str("msgID", msgID).Logger(),
		localNodePubKey: localNodePubKey,
		tssCommonStruct: common.NewTssCommon(localP2PID, broadcastChan, conf, msgID, privateKey, 1),
		stopChan:        stopChan,
		localParty:      nil,
		stateManager:    stateManager,
		commStopChan:    make(chan struct{}),
		p2pComm:         p2pComm,
	}
}

func (tKeyGen *EDDSAKeyGen) GetTssKeyGenChannels() chan *p2p.Message {
	return tKeyGen.tssCommonStruct.TssMsg
}

func (tKeyGen *EDDSAKeyGen) GetTssCommonStruct() *common.TssCommon {
	return tKeyGen.tssCommonStruct
}

func (tKeyGen *EDDSAKeyGen) GenerateNewKey(keygenReq keygen.Request) (*bcrypto.ECPoint, error) {
	partiesID, localPartyID, err := conversion.GetParties(keygenReq.Keys, tKeyGen.localNodePubKey, true, "")
	if err != nil {
		return nil, fmt.Errorf("fail to get keygen parties: %w", err)
	}

	keyGenLocalStateItem := storage.KeygenLocalState{
		ParticipantKeys: keygenReq.Keys,
		LocalPartyKey:   tKeyGen.localNodePubKey,
	}

	threshold, err := conversion.GetThreshold(len(partiesID))
	if err != nil {
		return nil, err
	}
	keyGenPartyMap := new(sync.Map)
	ctx := btss.NewPeerContext(partiesID)
	params := btss.NewParameters(btss.Edwards(), ctx, localPartyID, len(partiesID), threshold)
	outCh := make(chan btss.Message, len(partiesID))
	endCh := make(chan bkg.LocalPartySaveData, len(partiesID))
	errChan := make(chan struct{})
	blameMgr := tKeyGen.tssCommonStruct.GetBlameMgr()
	keyGenParty := bkg.NewLocalParty(params, outCh, endCh)
	partyIDMap := conversion.SetupPartyIDMap(partiesID)
	err1 := conversion.SetupIDMaps(partyIDMap, tKeyGen.tssCommonStruct.PartyIDtoP2PID)
	err2 := conversion.SetupIDMaps(partyIDMap, blameMgr.PartyIDtoP2PID)
	if err1 != nil || err2 != nil {
		tKeyGen.logger.Error().Msgf("[eddsa] error in creating mapping between partyID and P2P ID")
		return nil, err
	}
	// we never run multi keygen, so the moniker is set to default empty value
	keyGenPartyMap.Store("", keyGenParty)
	partyInfo := &common.PartyInfo{
		PartyMap:   keyGenPartyMap,
		PartyIDMap: partyIDMap,
	}

	tKeyGen.tssCommonStruct.SetPartyInfo(partyInfo)
	blameMgr.SetPartyInfo(keyGenPartyMap, partyIDMap)
	tKeyGen.tssCommonStruct.P2PPeersLock.Lock()
	tKeyGen.tssCommonStruct.P2PPeers = conversion.GetPeersID(tKeyGen.tssCommonStruct.PartyIDtoP2PID, tKeyGen.tssCommonStruct.GetLocalPeerID())
	tKeyGen.tssCommonStruct.P2PPeersLock.Unlock()
	var keyGenWg sync.WaitGroup
	keyGenWg.Add(2)
	// start keygen
	go func() {
		defer keyGenWg.Done()
		defer tKeyGen.logger.Debug().Msg("[eddsa] >>>>>>>>>>>>>.keyGenParty started")
		if err := keyGenParty.Start(); nil != err {
			tKeyGen.logger.Error().Err(err).Msg("[eddsa] fail to start keygen party")
			close(errChan)
		}
	}()
	go tKeyGen.tssCommonStruct.ProcessInboundMessages(tKeyGen.commStopChan, &keyGenWg)

	r, err, _ := tKeyGen.processKeyGen(errChan, outCh, endCh, keyGenLocalStateItem)
	if err != nil {
		close(tKeyGen.commStopChan)
		return nil, fmt.Errorf("fail to process key sign: %w", err)
	}
	select {
	case <-time.After(time.Second * 5):
		close(tKeyGen.commStopChan)

	case <-tKeyGen.tssCommonStruct.GetTaskDone():
		close(tKeyGen.commStopChan)
	}

	keyGenWg.Wait()
	return r, err
}

func (tKeyGen *EDDSAKeyGen) processKeyGen(errChan chan struct{},
	outCh <-chan btss.Message,
	endCh <-chan bkg.LocalPartySaveData,
	keyGenLocalStateItem storage.KeygenLocalState) (*bcrypto.ECPoint, error, string) {
	defer tKeyGen.logger.Debug().Msg("[eddsa] finished keygen process")
	tKeyGen.logger.Debug().Msg("[eddsa] start to read messages from local party")
	tssConf := tKeyGen.tssCommonStruct.GetConf()
	blameMgr := tKeyGen.tssCommonStruct.GetBlameMgr()
	for {
		select {
		case <-errChan: // when keyGenParty return
			tKeyGen.logger.Error().Msg("[eddsa] key gen failed")
			return nil, errors.New("[eddsa] error channel closed fail to start local party"), ""

		case <-tKeyGen.stopChan: // when TSS processor receive signal to quit
			return nil, errors.New("[eddsa] received exit signal"), ""

		case <-time.After(tssConf.KeyGenTimeout):
			// we bail out after KeyGenTimeoutSeconds
			tKeyGen.logger.Error().Msgf("[eddsa] fail to generate message with %s", tssConf.KeyGenTimeout.String())
			lastMsg := blameMgr.GetLastMsg()
			failReason := blameMgr.GetBlame().FailReason
			if failReason == "" {
				failReason = blame.TssTimeout
			}
			if lastMsg == nil {
				tKeyGen.logger.Error().Msg("[eddsa] fail to start the keygen, the last produced message of this node is none")
				return nil, errors.New("[eddsa] timeout before shared message is generated"), ""
			}
			blameNodesUnicast, err := blameMgr.GetUnicastBlame(messages.EDDSAKEYGEN2a)
			if err != nil {
				tKeyGen.logger.Error().Err(err).Msg("[eddsa] error in get unicast blame")
			}
			tKeyGen.tssCommonStruct.P2PPeersLock.RLock()
			threshold, err := conversion.GetThreshold(len(tKeyGen.tssCommonStruct.P2PPeers) + 1)
			tKeyGen.tssCommonStruct.P2PPeersLock.RUnlock()
			if err != nil {
				tKeyGen.logger.Error().Err(err).Msg("[eddsa] error in get the threshold to generate blame")
			}

			if len(blameNodesUnicast) > 0 && len(blameNodesUnicast) <= threshold {
				blameMgr.GetBlame().SetBlame(failReason, blameNodesUnicast, true, "KeyGenTimeout")
			}
			blameNodesBroadcast, err := blameMgr.GetBroadcastBlame(lastMsg.Type())
			if err != nil {
				tKeyGen.logger.Error().Err(err).Msg("[eddsa] error in get broadcast blame")
			}
			blameMgr.GetBlame().AddBlameNodes(blameNodesBroadcast...)

			// if we cannot find the blame node, we check whether everyone send me the share
			if len(blameMgr.GetBlame().BlameNodes) == 0 {
				blameNodesMisingShare, isUnicast, err := blameMgr.TssMissingShareBlame(messages.EDDSAKEYGENROUNDS, messages.EDDSAKEYGEN)
				if err != nil {
					tKeyGen.logger.Error().Err(err).Msg("[eddsa] fail to get the node of missing share ")
				}
				if len(blameNodesMisingShare) > 0 && len(blameNodesMisingShare) <= threshold {
					blameMgr.GetBlame().AddBlameNodes(blameNodesMisingShare...)
					blameMgr.GetBlame().IsUnicast = isUnicast
				}
			}
			return nil, blame.ErrTssTimeOut, ""

		case msg := <-outCh:
			tKeyGen.logger.Debug().Msgf("[eddsa] >>>>>>>>>>msg: %s", msg.String())
			blameMgr.SetLastMsg(msg)
			err := tKeyGen.tssCommonStruct.ProcessOutCh(msg, messages.TSSKeyGenMsg)
			if err != nil {
				tKeyGen.logger.Error().Err(err).Msg("[eddsa] fail to process the message")
				return nil, err, ""
			}

		case msg := <-endCh:
			tKeyGen.logger.Debug().Msgf("[eddsa] keygen finished successfully: %s", msg.EDDSAPub.Y().String())
			err := tKeyGen.tssCommonStruct.NotifyTaskDone()
			if err != nil {
				tKeyGen.logger.Error().Err(err).Msg("[eddsa] fail to broadcast the keysign done")
			}
			strPubKey, err := conversion.GetTssPubKeyEDDSA(msg.EDDSAPub)
			if err != nil {
				return nil, fmt.Errorf("fail to get thorchain pubkey: %w", err), ""
			}
			marshaledMsg, err := json.Marshal(msg)
			if err != nil {
				tKeyGen.logger.Error().Err(err).Msg("[eddsa] fail to marshal the result")
				return nil, errors.New("[eddsa] fail to marshal the result"), ""
			}
			keyGenLocalStateItem.LocalData = marshaledMsg
			keyGenLocalStateItem.PubKey = strPubKey
			if err := tKeyGen.stateManager.SaveLocalState(keyGenLocalStateItem, messages.EDDSAKEYGEN); err != nil {
				return nil, fmt.Errorf("[eddsa] fail to save keygen result to storage: %w", err), ""
			}
			address := tKeyGen.p2pComm.ExportPeerAddress()
			if err := tKeyGen.stateManager.SaveAddressBook(address); err != nil {
				tKeyGen.logger.Error().Err(err).Msg("[eddsa] fail to save the peer addresses")
			}
			return msg.EDDSAPub, nil, strPubKey
		}
	}
}
