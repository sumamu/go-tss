package tss

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	bc "github.com/binance-chain/tss-lib/common"

	"github.com/libp2p/go-libp2p-core/peer"

	"gitlab.com/thorchain/tss/go-tss/common"
	"gitlab.com/thorchain/tss/go-tss/keysign"
	"gitlab.com/thorchain/tss/go-tss/messages"
)

func (t *TssServer) KeySign(req keysign.Request) (keysign.Response, error) {
	t.logger.Info().Str("pool pub key", req.PoolPubKey).
		Str("signer pub keys", strings.Join(req.SignerPubKeys, ",")).
		Str("msg", strings.Join(req.Messages, ",")).
		Msg("received keysign request")
	emptyResp := keysign.Response{}
	msgID, err := t.requestToMsgId(req)
	if err != nil {
		return emptyResp, err
	}

	keysignInstance := keysign.NewTssKeySign(
		t.p2pCommunication.GetLocalPeerID(),
		t.conf,
		t.p2pCommunication.BroadcastMsgChan,
		t.stopChan,
		msgID,
		uint32(len(req.Messages)),
	)

	keySignChannels := keysignInstance.GetTssKeySignChannels()
	t.p2pCommunication.SetSubscribe(messages.TSSKeySignMsg, msgID, keySignChannels)
	t.p2pCommunication.SetSubscribe(messages.TSSKeySignVerMsg, msgID, keySignChannels)

	defer t.p2pCommunication.CancelSubscribe(messages.TSSKeySignMsg, msgID)
	defer t.p2pCommunication.CancelSubscribe(messages.TSSKeySignVerMsg, msgID)

	localStateItem, err := t.stateManager.GetLocalState(req.PoolPubKey)
	if err != nil {
		return emptyResp, fmt.Errorf("fail to get local keygen state: %w", err)
	}
	var msgsToSign [][]byte
	for _, val := range req.Messages {
		msgToSign, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			return keysign.Response{}, fmt.Errorf("fail to decode message(%s): %w", strings.Join(req.Messages, ","), err)
		}
		msgsToSign = append(msgsToSign, msgToSign)
	}
	if len(req.SignerPubKeys) == 0 {
		return emptyResp, errors.New("empty signer pub keys")
	}

	threshold, err := common.GetThreshold(len(localStateItem.ParticipantKeys))
	if err != nil {
		t.logger.Error().Err(err).Msg("fail to get the threshold")
		return emptyResp, errors.New("fail to get threshold")
	}
	if len(req.SignerPubKeys) <= threshold {
		t.logger.Error().Msgf("not enough signers, threshold=%d and signers=%d", threshold, len(req.SignerPubKeys))
		return emptyResp, errors.New("not enough signers")
	}

	if !t.isPartOfKeysignParty(req.SignerPubKeys) {
		// TSS keysign include both form party and keysign itself, thus we wait twice of the timeout
		data, err := t.signatureNotifier.WaitForSignature(msgID, msgToSign, req.PoolPubKey, t.conf.KeySignTimeout*2)
		if err != nil {
			return emptyResp, fmt.Errorf("fail to get signature:%w", err)
		}
		if len(data) == 0 {
			return emptyResp, errors.New("keysign failed")
		}
		return t.batchSignatures(msgID, data), nil
	}
	// get all the tss nodes that were part of the original key gen
	signers, err := GetPeerIDs(localStateItem.ParticipantKeys)
	if err != nil {
		return emptyResp, fmt.Errorf("fail to convert pub keys to peer id:%w", err)
	}
	sort.Strings(req.Messages)
	msgToSignID := strings.Join(req.Messages, ",")
	result, leaderPeerID, err := t.joinParty(msgID, []byte(msgToSignID), req.SignerPubKeys)
	if err != nil {
		// just blame the leader node
		pKey, err := GetPubKeyFromPeerID(leaderPeerID.String())
		if err != nil {
			t.logger.Error().Err(err).Msg("fail to extract pub key from peer ID")
		}
		t.broadcastKeysignFailure(msgID, signers)
		if result != nil {
			t.logger.Error().Err(err).Msgf("fail to form keygen party-x: %s", result.Type)
		}
		return keysign.Response{
			Status: common.Fail,
			Blame:  common.NewBlame(common.BlameTssCoordinator, []string{pKey}),
		}, nil
	}
	if result.Type != messages.JoinPartyResponse_Success {
		pKey, err := GetPubKeyFromPeerID(leaderPeerID.String())
		if err != nil {
			t.logger.Error().Err(err).Msg("fail to extract pub key from peer ID")
		}
		blame, err := t.getBlamePeers(req.SignerPubKeys, result.PeerIDs, common.BlameTssSync)
		if err != nil {
			t.logger.Err(err).Msg("fail to get peers to blame")
		}
		t.broadcastKeysignFailure(msgID, signers)
		// make sure we blame the leader as well
		blame.AddBlameNodes(pKey)
		t.logger.Error().Err(err).Msgf("fail to form keysign party:%s", result.Type)
		return keysign.Response{
			Status: common.Fail,
			Blame:  blame,
		}, nil
	}

	signaturesData, err := keysignInstance.SignMessage(msgsToSign, localStateItem, req.SignerPubKeys)
	// the statistic of keygen only care about Tss it self, even if the following http response aborts,
	// it still counted as a successful keygen as the Tss model runs successfully.
	if err != nil {
		t.logger.Error().Err(err).Msg("err in keysign")
		atomic.AddUint64(&t.Status.FailedKeySign, 1)
		t.broadcastKeysignFailure(msgID, signers)
		return keysign.Response{
			Status: common.Fail,
			Blame:  keysignInstance.GetTssCommonStruct().BlamePeers,
		}, nil
	}

	atomic.AddUint64(&t.Status.SucKeySign, 1)

	// update signature notification
	if err := t.signatureNotifier.BroadcastSignature(msgID, signaturesData, signers); err != nil {
		return emptyResp, fmt.Errorf("fail to broadcast signature:%w", err)
	}
	return t.batchSignatures(msgID, signaturesData), nil
}

func (t *TssServer) batchSignatures(msgID string, sigs []*bc.SignatureData) keysign.Response {
	var signatures []keysign.Signature
	for _, sig := range sigs {
		msg := base64.StdEncoding.EncodeToString(sig.GetM())
		R := base64.StdEncoding.EncodeToString(sig.GetR())
		S := base64.StdEncoding.EncodeToString(sig.GetS())
		signature := keysign.NewSignature(msg, R, S)
		signatures = append(signatures, signature)
	}
	return keysign.NewResponse(
		msgID,
		signatures,
		common.Success,
		common.NoBlame,
	)

}

func (t *TssServer) broadcastKeysignFailure(messageID string, peers []peer.ID) {
	if err := t.signatureNotifier.BroadcastFailed(messageID, peers); err != nil {
		t.logger.Err(err).Msg("fail to broadcast keysign failure")
	}
}

func (t *TssServer) isPartOfKeysignParty(parties []string) bool {
	for _, item := range parties {
		if t.localNodePubKey == item {
			return true
		}
	}
	return false
}
