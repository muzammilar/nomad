// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package structs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper/uuid"
)

const (
	// VariablesApplyRPCMethod is the RPC method for upserting or deleting a
	// variable by its namespace and path, with optional conflict detection.
	//
	// Args: VariablesApplyRequest
	// Reply: VariablesApplyResponse
	VariablesApplyRPCMethod = "Variables.Apply"

	// VariablesListRPCMethod is the RPC method for listing variables within
	// Nomad.
	//
	// Args: VariablesListRequest
	// Reply: VariablesListResponse
	VariablesListRPCMethod = "Variables.List"

	// VariablesReadRPCMethod is the RPC method for fetching a variable
	// according to its namespace and path.
	//
	// Args: VariablesByNameRequest
	// Reply: VariablesByNameResponse
	VariablesReadRPCMethod = "Variables.Read"

	// VariablesRenewLockRPCMethod is the RPC method for renewing the lease on
	// a lock according to its namespace, path and lock ID.
	//
	// Args: VariablesRenewLockRequest
	// Reply: VariablesRenewLockResponse
	VariablesRenewLockRPCMethod = "Variables.RenewLock"

	// maxVariableSize is the maximum size of the unencrypted contents of a
	// variable. This size is deliberately set low and is not configurable, to
	// discourage DoS'ing the cluster
	maxVariableSize = 65536

	// minVariableLockTTL and maxVariableLockTTL determine the range of valid durations for the
	// TTL on a lock.They come from the experience on Consul.
	minVariableLockTTL = 10 * time.Second
	maxVariableLockTTL = 24 * time.Hour

	// defaultLockTTL is the default value used to maintain a lock before it needs to
	// be renewed. The actual value comes from the experience with Consul.
	defaultLockTTL = 15 * time.Second

	// defaultLockDelay is the default a lock will be blocked after the TTL
	// went by without any renews. It is intended to prevent split brain situations.
	// The actual value comes from the experience with Consul.
	defaultLockDelay = 15 * time.Second
)

var (
	errNoPath             = errors.New("missing path")
	errNoNamespace        = errors.New("missing namespace")
	errNoLock             = errors.New("missing lock ID")
	errWildCardNamespace  = errors.New("can not target wildcard (\"*\")namespace")
	errQuotaExhausted     = errors.New("variables are limited to 64KiB in total size")
	errNegativeDelayOrTTL = errors.New("Lock delay and TTL must be positive")
	errInvalidTTL         = errors.New("TTL must be between 10 seconds and 24 hours")
)

// VariableMetadata is the metadata envelope for a Variable, it is the list
// object and is shared data between an VariableEncrypted and a
// VariableDecrypted object.
type VariableMetadata struct {
	Namespace string
	Path      string

	// Lock represents a variable which is used for locking functionality.
	Lock *VariableLock `json:",omitempty"`

	CreateIndex uint64
	CreateTime  int64
	ModifyIndex uint64
	ModifyTime  int64
}

// VariableEncrypted structs are returned from the Encrypter's encrypt
// method. They are the only form that should ever be persisted to storage.
type VariableEncrypted struct {
	VariableMetadata
	VariableData
}

// VariableData is the secret data for a Variable
type VariableData struct {
	Data  []byte // includes nonce
	KeyID string // ID of root key used to encrypt this entry
}

// VariableDecrypted structs are returned from the Encrypter's decrypt
// method. Since they contain sensitive material, they should never be
// persisted to disk.
type VariableDecrypted struct {
	VariableMetadata
	Items VariableItems `json:",omitempty"`
}

// VariableItems are the actual secrets stored in a variable. They are always
// encrypted and decrypted as a single unit.
type VariableItems map[string]string

// VariableLock represent a Nomad variable which is used to performing locking
// functionality such as leadership election.
type VariableLock struct {

	// ID is generated by Nomad to provide a unique caller ID which can be used
	// for renewals and unlocking.
	ID string

	// TTL describes the time-to-live of the current lock holder. The client
	// must renew the lock before this TTL expires, otherwise the lock is
	// considered lost.
	TTL time.Duration

	// LockDelay describes a grace period that exists after a lock is lost,
	// before another client may acquire the lock. This helps protect against
	// split-brains.
	LockDelay time.Duration
}

// Equal performs an equality check on the two variable lock objects. It
// handles nil objects.
func (vl *VariableLock) Equal(vl2 *VariableLock) bool {
	if vl == nil || vl2 == nil {
		return vl == vl2
	}
	if vl.ID != vl2.ID {
		return false
	}
	if vl.TTL != vl2.TTL {
		return false
	}
	if vl.LockDelay != vl2.LockDelay {
		return false
	}
	return true
}

// MarshalJSON implements the json.Marshaler interface and allows
// VariableLock.TTL and VariableLock.Delay to be marshaled correctly.
func (vl *VariableLock) MarshalJSON() ([]byte, error) {
	type Alias VariableLock
	exported := &struct {
		TTL       string
		LockDelay string
		*Alias
	}{
		TTL:       vl.TTL.String(),
		LockDelay: vl.LockDelay.String(),
		Alias:     (*Alias)(vl),
	}

	if vl.TTL == 0 {
		exported.TTL = ""
	}

	if vl.LockDelay == 0 {
		exported.LockDelay = ""
	}
	return json.Marshal(exported)
}

// UnmarshalJSON implements the json.Unmarshaler interface and allows
// VariableLock.TTL and VariableLock.Delay to be unmarshalled correctly.
func (vl *VariableLock) UnmarshalJSON(data []byte) (err error) {
	type Alias VariableLock
	aux := &struct {
		TTL       interface{}
		LockDelay interface{}
		*Alias
	}{
		Alias: (*Alias)(vl),
	}

	if err = json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if aux.TTL != nil {
		switch v := aux.TTL.(type) {
		case string:
			if v != "" {
				if vl.TTL, err = time.ParseDuration(v); err != nil {
					return err
				}
			}
		case float64:
			vl.TTL = time.Duration(v)
		}

	}

	if aux.LockDelay != nil {
		switch v := aux.LockDelay.(type) {
		case string:
			if v != "" {
				if vl.LockDelay, err = time.ParseDuration(v); err != nil {
					return err
				}
			}
		case float64:
			vl.LockDelay = time.Duration(v)
		}

	}

	return nil
}

// Copy creates a deep copy of the variable lock. This copy can then be safely
// modified. It handles nil objects.
func (vl *VariableLock) Copy() *VariableLock {
	if vl == nil {
		return nil
	}

	nvl := new(VariableLock)
	*nvl = *vl

	return nvl
}

func (vl *VariableLock) Canonicalize() {
	// If the lock ID is empty, it means this is a creation of a lock on this variable.
	if vl.ID == "" {
		vl.ID = uuid.Generate()
	}

	if vl.LockDelay == 0 {
		vl.LockDelay = defaultLockDelay
	}

	if vl.TTL == 0 {
		vl.TTL = defaultLockTTL
	}
}

func (vl *VariableLock) Validate() error {
	var mErr *multierror.Error

	if vl.LockDelay < 0 || vl.TTL < 0 {
		mErr = multierror.Append(mErr, errNegativeDelayOrTTL)
	}

	if vl.TTL > maxVariableLockTTL || vl.TTL < minVariableLockTTL {
		mErr = multierror.Append(mErr, errInvalidTTL)
	}

	return mErr.ErrorOrNil()
}

func (vi VariableItems) Size() uint64 {
	var out uint64
	for k, v := range vi {
		out += uint64(len(k))
		out += uint64(len(v))
	}
	return out
}

// Equal checks both the metadata and items in a VariableDecrypted struct
func (vd VariableDecrypted) Equal(v2 VariableDecrypted) bool {
	return vd.VariableMetadata.Equal(v2.VariableMetadata) &&
		vd.Items.Equal(v2.Items)
}

// Equal is a convenience method to provide similar equality checking syntax
// for metadata and the VariablesData or VariableItems struct
func (sv VariableMetadata) Equal(vm2 VariableMetadata) bool {
	if sv.Namespace != vm2.Namespace {
		return false
	}
	if sv.Path != vm2.Path {
		return false
	}
	if sv.CreateIndex != vm2.CreateIndex {
		return false
	}
	if sv.CreateTime != vm2.CreateTime {
		return false
	}
	if sv.ModifyIndex != vm2.ModifyIndex {
		return false
	}
	if sv.ModifyTime != vm2.ModifyTime {
		return false
	}
	return sv.Lock.Equal(vm2.Lock)
}

// Equal performs deep equality checking on the cleartext items of a
// VariableDecrypted. Uses reflect.DeepEqual
func (vi VariableItems) Equal(i2 VariableItems) bool {
	return reflect.DeepEqual(vi, i2)
}

// Equal checks both the metadata and encrypted data for a VariableEncrypted
// struct
func (ve VariableEncrypted) Equal(v2 VariableEncrypted) bool {
	return ve.VariableMetadata.Equal(v2.VariableMetadata) &&
		ve.VariableData.Equal(v2.VariableData)
}

// Equal performs deep equality checking on the encrypted data part of a
// VariableEncrypted
func (vd VariableData) Equal(d2 VariableData) bool {
	return vd.KeyID == d2.KeyID &&
		bytes.Equal(vd.Data, d2.Data)
}

func (vd VariableDecrypted) Copy() VariableDecrypted {
	return VariableDecrypted{
		VariableMetadata: vd.VariableMetadata,
		Items:            vd.Items.Copy(),
	}
}

func (vi VariableItems) Copy() VariableItems {
	out := make(VariableItems, len(vi))
	for k, v := range vi {
		out[k] = v
	}
	return out
}

func (ve VariableEncrypted) Copy() VariableEncrypted {
	return VariableEncrypted{
		VariableMetadata: ve.VariableMetadata,
		VariableData:     ve.VariableData.Copy(),
	}
}

func (vd VariableData) Copy() VariableData {
	out := make([]byte, len(vd.Data))
	copy(out, vd.Data)
	return VariableData{
		Data:  out,
		KeyID: vd.KeyID,
	}
}

var (
	// validVariablePath is used to validate a variable path. We restrict to
	// RFC3986 URL-safe characters that don't conflict with the use of
	// characters "@" and "." in template blocks. We also restrict the length so
	// that a user can't make queries in the state store unusually expensive (as
	// they are O(k) on the key length)
	validVariablePath = regexp.MustCompile("^[a-zA-Z0-9-_~/]{1,128}$")
)

func (vd VariableDecrypted) Validate() error {

	if vd.Namespace == AllNamespacesSentinel {
		return errors.New("can not target wildcard (\"*\")namespace")
	}

	if len(vd.Items) == 0 {
		return errors.New("empty variables are invalid")
	}

	if vd.Items.Size() > maxVariableSize {
		return errors.New("variables are limited to 64KiB in total size")
	}

	if err := ValidatePath(vd.Path); err != nil {
		return err
	}

	if vd.Lock != nil {
		return vd.Lock.Validate()
	}

	return nil
}

// ValidateForLock ensures a new variable can be created just to support a lock,
// it doesn't require to hold any items and it will validate the lock.
func (vd VariableDecrypted) ValidateForLock() error {
	var mErr multierror.Error
	if vd.Namespace == AllNamespacesSentinel {
		mErr.Errors = append(mErr.Errors, errWildCardNamespace)
		return &mErr
	}

	if vd.Items.Size() > maxVariableSize {
		return errQuotaExhausted
	}

	if err := ValidatePath(vd.Path); err != nil {
		return err
	}

	return vd.Lock.Validate()
}

func ValidatePath(path string) error {
	if len(path) == 0 {
		return fmt.Errorf("variable requires path")
	}
	if !validVariablePath.MatchString(path) {
		return fmt.Errorf("invalid path %q", path)
	}

	parts := strings.Split(path, "/")

	if parts[0] != "nomad" {
		return nil
	}

	// Don't allow a variable with path "nomad"
	if len(parts) == 1 {
		return fmt.Errorf("\"nomad\" is a reserved top-level directory path, but you may write variables to \"nomad/jobs\", \"nomad/job-templates\", or below")
	}

	switch {
	case parts[1] == "jobs":
		// Any path including "nomad/jobs" is valid
		return nil
	case parts[1] == "job-templates" && len(parts) == 3:
		// Paths including "nomad/job-templates" is valid, provided they have single further path part
		return nil
	case parts[1] == "job-templates":
		// Disallow exactly nomad/job-templates with no further paths
		return fmt.Errorf("\"nomad/job-templates\" is a reserved directory path, but you may write variables at the level below it, for example, \"nomad/job-templates/template-name\"")
	default:
		// Disallow arbitrary sub-paths beneath nomad/
		return fmt.Errorf("only paths at \"nomad/jobs\" or \"nomad/job-templates\" and below are valid paths under the top-level \"nomad\" directory")
	}
}

func (vd *VariableDecrypted) Canonicalize() {
	if vd.Namespace == "" {
		vd.Namespace = DefaultNamespace
	}

	if vd.Lock != nil {
		vd.Lock.Canonicalize()
	}
}

// Copy returns a fully hydrated copy of VariableMetadata that can be
// manipulated while ensuring the original is not touched.
func (sv *VariableMetadata) Copy() *VariableMetadata {
	if sv == nil {
		return nil
	}

	nsl := new(VariableMetadata)
	*nsl = *sv

	if sv.Lock != nil {
		nsl.Lock = sv.Lock.Copy()
	}

	return nsl
}

// GetNamespace returns the variable's namespace. Used for pagination.
func (sv VariableMetadata) GetNamespace() string {
	return sv.Namespace
}

// GetID returns the variable's path. Used for pagination.
func (sv VariableMetadata) GetID() string {
	return sv.Path
}

// GetCreateIndex returns the variable's create index. Used for pagination.
func (sv VariableMetadata) GetCreateIndex() uint64 {
	return sv.CreateIndex
}

// LockID returns the ID of the lock. In the event this is not held, or the
// variable is not a lock, this string will be empty.
func (sv *VariableMetadata) LockID() string {
	if sv.Lock == nil {
		return ""
	}
	return sv.Lock.ID
}

// IsLock is a helper to indicate whether the variable is being used for
// locking.
func (sv *VariableMetadata) IsLock() bool { return sv.Lock != nil }

// VariablesQuota is used to track the total size of variables entries per
// namespace. The total length of Variable.EncryptedData in bytes will be added
// to the VariablesQuota table in the same transaction as a write, update, or
// delete. This tracking effectively caps the maximum size of variables in a
// given namespace to MaxInt64 bytes.
type VariablesQuota struct {
	Namespace   string
	Size        int64
	CreateIndex uint64
	ModifyIndex uint64
}

func (svq *VariablesQuota) Copy() *VariablesQuota {
	if svq == nil {
		return nil
	}
	nq := new(VariablesQuota)
	*nq = *svq
	return nq
}

// ---------------------------------------
// RPC and FSM request/response objects

// VarOp constants give possible operations available in a transaction.
type VarOp string

const (
	VarOpSet       VarOp = "set"
	VarOpDelete    VarOp = "delete"
	VarOpDeleteCAS VarOp = "delete-cas"
	VarOpCAS       VarOp = "cas"

	// VarOpLockAcquire is the variable operation used when attempting to
	// acquire a variable lock.
	VarOpLockAcquire VarOp = "lock-acquire"

	// VarOpLockRelease is the variable operation used when attempting to
	// release a held variable lock.
	VarOpLockRelease VarOp = "lock-release"
)

// VarOpResult constants give possible operations results from a transaction.
type VarOpResult string

const (
	VarOpResultOk       VarOpResult = "ok"
	VarOpResultConflict VarOpResult = "conflict"
	VarOpResultRedacted VarOpResult = "conflict-redacted"
	VarOpResultError    VarOpResult = "error"
)

// VariablesApplyRequest is used by users to operate on the variable store
type VariablesApplyRequest struct {
	Op  VarOp              // Operation to be performed during apply
	Var *VariableDecrypted // Variable-shaped request data
	WriteRequest
}

// VariablesApplyResponse is sent back to the user to inform them of success or failure
type VariablesApplyResponse struct {
	Op       VarOp              // Operation performed
	Input    *VariableDecrypted // Input supplied
	Result   VarOpResult        // Return status from operation
	Error    error              // Error if any
	Conflict *VariableDecrypted // Conflicting value if applicable
	Output   *VariableDecrypted // Operation Result if successful; nil for successful deletes
	WriteMeta
}

func (r *VariablesApplyResponse) IsOk() bool {
	return r.Result == VarOpResultOk
}

func (r *VariablesApplyResponse) IsConflict() bool {
	return r.Result == VarOpResultConflict || r.Result == VarOpResultRedacted
}

func (r *VariablesApplyResponse) IsError() bool {
	return r.Result == VarOpResultError
}

func (r *VariablesApplyResponse) IsRedacted() bool {
	return r.Result == VarOpResultRedacted
}

// VarApplyStateRequest is used by the FSM to modify the variable store
type VarApplyStateRequest struct {
	Op  VarOp              // Which operation are we performing
	Var *VariableEncrypted // Which directory entry
	WriteRequest
}

// VarApplyStateResponse is used by the FSM to inform the RPC layer of success or failure
type VarApplyStateResponse struct {
	Op            VarOp              // Which operation were we performing
	Result        VarOpResult        // What happened (ok, conflict, error)
	Error         error              // error if any
	Conflict      *VariableEncrypted // conflicting variable if applies
	WrittenSVMeta *VariableMetadata  // for making the VariablesApplyResponse
	WriteMeta
}

func (r *VarApplyStateRequest) ErrorResponse(raftIndex uint64, err error) *VarApplyStateResponse {
	return &VarApplyStateResponse{
		Op:        r.Op,
		Result:    VarOpResultError,
		Error:     err,
		WriteMeta: WriteMeta{Index: raftIndex},
	}
}

func (r *VarApplyStateRequest) SuccessResponse(raftIndex uint64, meta *VariableMetadata) *VarApplyStateResponse {
	return &VarApplyStateResponse{
		Op:            r.Op,
		Result:        VarOpResultOk,
		WrittenSVMeta: meta,
		WriteMeta:     WriteMeta{Index: raftIndex},
	}
}

func (r *VarApplyStateRequest) ConflictResponse(raftIndex uint64, cv *VariableEncrypted) *VarApplyStateResponse {
	var cvCopy VariableEncrypted
	if cv != nil {
		// make a copy so that we aren't sending
		// the live state store version
		cvCopy = cv.Copy()
	}
	return &VarApplyStateResponse{
		Op:        r.Op,
		Result:    VarOpResultConflict,
		Conflict:  &cvCopy,
		WriteMeta: WriteMeta{Index: raftIndex},
	}
}

func (r *VarApplyStateResponse) IsOk() bool {
	return r.Result == VarOpResultOk
}

func (r *VarApplyStateResponse) IsConflict() bool {
	return r.Result == VarOpResultConflict
}

func (r *VarApplyStateResponse) IsError() bool {
	// FIXME: This is brittle and requires immense faith that
	// the response is properly managed.
	return r.Result == VarOpResultError
}

type VariablesListRequest struct {
	QueryOptions
}

type VariablesListResponse struct {
	Data []*VariableMetadata
	QueryMeta
}

type VariablesReadRequest struct {
	Path string
	QueryOptions
}

type VariablesReadResponse struct {
	Data *VariableDecrypted
	QueryMeta
}

// VariablesRenewLockRequest is used to renew the lease on a lock. This request
// behaves like a write because the renewal needs to be forwarded to the leader
// where the timers and lock work is kept.
type VariablesRenewLockRequest struct {
	//Namespace string
	Path   string
	LockID string

	WriteRequest
}

func (v *VariablesRenewLockRequest) Validate() error {
	var mErr multierror.Error

	if v.Path == "" {
		mErr.Errors = append(mErr.Errors, errNoPath)
	}
	if v.LockID == "" {
		mErr.Errors = append(mErr.Errors, errNoLock)
	}

	return mErr.ErrorOrNil()
}

// VariablesRenewLockResponse is sent back to the user to inform them of success or failure
// of the renewal process.
type VariablesRenewLockResponse struct {
	VarMeta *VariableMetadata
	WriteMeta
}
