package msg

// HeaderRefused is the header a node sets on a refusal: one stable reason word, so a client can
// say in its own words what happened and what to do next, whatever the node's text says. A node
// from before it sets none, and the client tells the reason from the status and text instead.
const HeaderRefused = "X-Aivoicebus-Refused"

// Why a node refused a request. The words are a wire format: never rename one.
const (
	RefusedToken        = "token"           // the token is not one the node knows (401)
	RefusedDenied       = "denied"          // a machine token acting as another machine (403)
	RefusedReadOnly     = "read_only"       // an observer or machine token asking for what it may not (403)
	RefusedUnsigned     = "unsigned"        // the node takes only signed messages (403)
	RefusedSecret       = "secret"          // the body looks like it holds a credential, an approver signature or a pipe link with its key (422)
	RefusedApproval     = "approval"        // a reply to an approval request that cannot be the person's signed decision (403)
	RefusedTurns        = "turns"           // the conversation used up its --max-turns (409)
	RefusedDeadline     = "deadline"        // the conversation's --deadline passed (409)
	RefusedLoop         = "loop"            // the same thing said again and again in one thread (409)
	RefusedBudget       = "budget"          // the conversation used up its --budget-chars (409)
	RefusedRateSender   = "rate_sender"     // a sender's messages for the hour are used up (429)
	RefusedRateMachine  = "rate_machine"    // a machine's messages for the hour are used up (429)
	RefusedRatePair     = "rate_pair"       // two sessions answering each other in a loop (429)
	RefusedRateRetract  = "rate_retract"    // an identity's retractions for the hour are used up (429)
	RefusedUnknown      = "unknown"         // the node does not hold the message named (422)
	RefusedNotSender    = "not_sender"      // only a message's sender (or its machine) may retract it (403)
	RefusedReservedID   = "reserved_id"     // the id is a retraction notice's (400)
	RefusedNotice       = "reply_to_notice" // a reply to a retraction notice (400)
	RefusedIDTaken      = "id_taken"        // another message holds the id the message was sent under (409)
	RefusedBadRequest   = "bad_request"     // the request is malformed: usually a client older or newer than the node (400)
	RefusedTooLarge     = "too_large"       // a file larger than a node takes (413)
	RefusedGone         = "gone"            // a file that expired or was already read (404)
	RefusedRecovering   = "recovering"      // the node is catching up with its peers (503)
	RefusedShareFull    = "share_full"      // a shared space, or the node, holds as much as it may (507)
	RefusedConflict     = "conflict"        // a write names a version that is no longer the latest (409)
	RefusedShareMissing = "share_missing"   // a version names chunks the node does not hold (400)
)
