package session

// Capabilities describes operations an external Agent session has verified.
type Capabilities struct {
	CanStart       bool `json:"can_start"`
	CanDiscover    bool `json:"can_discover"`
	CanAttach      bool `json:"can_attach"`
	CanObserve     bool `json:"can_observe"`
	CanSendInput   bool `json:"can_send_input"`
	CanStream      bool `json:"can_stream"`
	CanInterrupt   bool `json:"can_interrupt"`
	CanResume      bool `json:"can_resume"`
	CanApprove     bool `json:"can_approve"`
	CanReadHistory bool `json:"can_read_history"`
}
