package swap

import (
	"encoding/hex"
	"errors"
	"log"
)

type SwapCreationContext struct {
	swapId          string
	asset           string
	amount          uint64
	peer            string
	channelId       string
	initiatorId     string
	protocolversion uint64
}

func (c *SwapCreationContext) ApplyOnSwap(swap *SwapData) {
	swap.Amount = c.amount
	swap.PeerNodeId = c.peer
	swap.ChannelId = c.channelId
	swap.Asset = c.asset
	swap.Id = c.swapId
	swap.InitiatorNodeId = c.initiatorId
	swap.ProtocolVersion = c.protocolversion
}

// SwapInSenderInitAction creates the swap data
type CreateSwapOutAction struct{}

//todo validate data
func (a *CreateSwapOutAction) Execute(services *SwapServices, swap *SwapData) EventType {
	newSwap := NewSwap(swap.Id, swap.Asset, SWAPTYPE_OUT, SWAPROLE_SENDER, swap.Amount, swap.InitiatorNodeId, swap.PeerNodeId, swap.ChannelId, swap.ProtocolVersion)
	*swap = *newSwap

	pubkey := swap.GetPrivkey().PubKey()
	swap.TakerPubkeyHash = hex.EncodeToString(pubkey.SerializeCompressed())

	swap.NextMessage = SwapOutRequest{
		SwapId:          swap.Id,
		ChannelId:       swap.ChannelId,
		Amount:          swap.Amount,
		TakerPubkeyHash: swap.TakerPubkeyHash,
		ProtocolVersion: swap.ProtocolVersion,
		Asset:           swap.Asset,
	}
	return Event_ActionSucceeded
}

type SendMessageAction struct{}

func (s *SendMessageAction) Execute(services *SwapServices, swap *SwapData) EventType {
	if swap.NextMessage == nil {
		return swap.HandleError(errors.New("swap.NextMessage is nil"))
	}
	err := services.messenger.SendMessage(swap.PeerNodeId, swap.NextMessage)
	if err != nil {
		return swap.HandleError(err)
	}
	return Event_ActionSucceeded
}

// PayFeeInvoiceAction checks the feeinvoice and pays it
type PayFeeInvoiceAction struct{}

func (r *PayFeeInvoiceAction) Execute(services *SwapServices, swap *SwapData) EventType {
	ll := services.lightning
	// policy := services.policy
	invoice, err := ll.DecodePayreq(swap.FeeInvoice)
	if err != nil {

		log.Printf("error decoding %v", err)
		return Event_ActionFailed
	}
	swap.OpeningTxFee = invoice.Amount / 1000
	// todo check peerId
	/*
		if !policy.ShouldPayFee(swap.Amount, invoice.Amount, swap.PeerNodeId, swap.ChannelId) {

			log.Printf("won't pay fee %v", err)
			return Event_ActionFailed
		}
	*/
	preimage, err := ll.PayInvoice(swap.FeeInvoice)
	if err != nil {

		log.Printf("error paying out %v", err)
		return Event_ActionFailed
	}
	swap.FeePreimage = preimage
	return Event_ActionSucceeded
}

// AwaitTxConfirmationAction  checks the claim invoice and adds the transaction to the txwatcher
type AwaitTxConfirmationAction struct{}

//todo this will not ever throw an error
func (t *AwaitTxConfirmationAction) Execute(services *SwapServices, swap *SwapData) EventType {
	onchain, err := services.getOnchainAsset(swap.Asset)
	if err != nil {
		return Event_ActionFailed
	}

	// todo check policy

	err = onchain.AddWaitForConfirmationTx(swap.Id, swap.OpeningTxId)
	if err != nil {
		return Event_ActionFailed
	}
	return NoOp
}

// todo

// ValidateTxAndPayClaimInvoiceAction pays the claim invoice
type ValidateTxAndPayClaimInvoiceAction struct{}

func (p *ValidateTxAndPayClaimInvoiceAction) Execute(services *SwapServices, swap *SwapData) EventType {
	lc := services.lightning
	onchain, err := services.getOnchainAsset(swap.Asset)
	if err != nil {
		return Event_ActionFailed
	}

	invoice, err := lc.DecodePayreq(swap.ClaimInvoice)
	if err != nil {
		return Event_ActionFailed
	}

	// todo this might fail, msats...
	if invoice.Amount != swap.Amount {
		return swap.HandleError(errors.New("invoice amount does not equal swap amount"))
	}

	swap.ClaimPaymentHash = invoice.PHash

	ok, err := onchain.ValidateTx(swap.GetOpeningParams(), swap.Cltv, swap.OpeningTxId)
	if err != nil {
		return Event_ActionFailed
	}
	if !ok {
		return Event_ActionFailed
	}
	preimageString, err := lc.RebalancePayment(swap.ClaimInvoice, swap.ChannelId)
	if err != nil {
		return Event_ActionFailed
	}
	swap.ClaimPreimage = preimageString
	return Event_ActionSucceeded
}

type NoOpAction struct{}

func (n *NoOpAction) Execute(services *SwapServices, swap *SwapData) EventType {
	return NoOp
}

type NoOpDoneAction struct{}

func (a *NoOpDoneAction) Execute(services *SwapServices, swap *SwapData) EventType {
	return Event_Done
}

// swapOutSenderFromStore recovers a swap statemachine from the swap store
func swapOutSenderFromStore(smData *SwapStateMachine, services *SwapServices) *SwapStateMachine {
	smData.swapServices = services
	smData.States = getSwapOutSenderStates()
	return smData
}

// newSwapOutSenderFSM returns a new swap statemachine for a swap-out sender
func newSwapOutSenderFSM(services *SwapServices) *SwapStateMachine {
	return &SwapStateMachine{
		Id:           newSwapId(),
		swapServices: services,
		Type:         SWAPTYPE_OUT,
		Role:         SWAPROLE_SENDER,
		States:       getSwapOutSenderStates(),
		Data:         &SwapData{},
	}
}

// getSwapOutSenderStates returns the states for the swap-out sender
func getSwapOutSenderStates() States {
	return States{
		Default: State{
			Events: Events{
				Event_OnSwapOutStarted: State_SwapOutSender_CreateSwap,
			},
		},
		State_SwapOutSender_CreateSwap: {
			Action: &CreateSwapOutAction{},
			Events: Events{
				Event_ActionSucceeded: State_SwapOutSender_SendRequest,
			},
		},
		State_SwapOutSender_SendRequest: {
			Action: &SendMessageAction{},
			Events: Events{
				Event_ActionFailed:    State_SwapCanceled,
				Event_ActionSucceeded: State_SwapOutSender_AwaitFeeResponse,
			},
		},
		State_SwapOutSender_AwaitFeeResponse: {
			Action: &NoOpAction{},
			Events: Events{
				Event_OnCancelReceived:     State_SwapCanceled,
				Event_OnFeeInvoiceReceived: State_SwapOutSender_PayFeeInvoice,
			},
		},
		State_SwapOutSender_PayFeeInvoice: {
			Action: &PayFeeInvoiceAction{},
			Events: Events{
				Event_ActionFailed:    State_SendCancel,
				Event_ActionSucceeded: State_SwapOutSender_AwaitTxBroadcastedMessage,
			},
		},
		State_SwapOutSender_AwaitTxBroadcastedMessage: {
			Action: &NoOpAction{},
			Events: Events{
				Event_OnCancelReceived:  State_SwapCanceled,
				Event_OnTxOpenedMessage: State_SwapOutSender_AwaitTxConfirmation,
			},
		},
		State_SendCancel: {
			Action: &SendCancelAction{},
			Events: Events{
				Event_ActionSucceeded: State_SwapCanceled,
				Event_ActionFailed:    State_SwapCanceled,
			},
		},
		State_SwapOutSender_AwaitTxConfirmation: {
			Action: &AwaitTxConfirmationAction{},
			Events: Events{
				Event_ActionFailed:  State_SendCancel,
				Event_OnTxConfirmed: State_SwapOutSender_ValidateTxAndPayClaimInvoice,
			},
		},
		State_SwapOutSender_ValidateTxAndPayClaimInvoice: {
			Action: &ValidateTxAndPayClaimInvoiceAction{},
			Events: Events{
				Event_ActionFailed:    State_SendCancel,
				Event_ActionSucceeded: State_SwapOutSender_ClaimSwap,
			},
		},
		State_SwapOutSender_ClaimSwap: {
			Action: &ClaimSwapTransactionWithPreimageAction{},
			Events: Events{
				Event_ActionSucceeded: State_SwapOutSender_SendClaimMessage,
				Event_OnRetry:         State_SwapOutSender_ClaimSwap,
			},
		},
		State_SwapOutSender_SendClaimMessage: {
			Action: &SendMessageAction{},
			Events: Events{
				Event_ActionSucceeded: State_ClaimedPreimage,
				Event_ActionFailed:    State_ClaimedPreimage,
			},
		},
		State_SwapCanceled: {
			Action: &CancelAction{},
		},
		State_ClaimedPreimage: {
			Action: &NoOpDoneAction{},
		},
	}
}
