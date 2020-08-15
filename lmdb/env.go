package lmdb

/*
#include <stdlib.h>
#include <stdio.h>
#include "lmdb.h"
#include "lmdbgo.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"github.com/glycerine/idem"
)

// success is a value returned from the LMDB API to indicate a successful call.
// The functions in this API this behavior and its use is not required.
const success = C.MDB_SUCCESS

// These flags are used exclusively for Env types and are set with Env.Open.
// Some flags may be set/unset later using Env.Set/.Unset methods.  Others will
// produce syscall.EINVAL.  Refer to the C documentation for detailed
// information.
const (
	// Flags for Env.Open.
	//
	// See mdb_env_open

	FixedMap    = C.MDB_FIXEDMAP   // Danger zone. Map memory at a fixed address.
	NoSubdir    = C.MDB_NOSUBDIR   // Argument to Open is a file, not a directory.
	Readonly    = C.MDB_RDONLY     // Used in several functions to denote an object as readonly.
	WriteMap    = C.MDB_WRITEMAP   // Use a writable memory map.
	NoMetaSync  = C.MDB_NOMETASYNC // Don't fsync metapage after commit.
	NoSync      = C.MDB_NOSYNC     // Don't fsync after commit.
	MapAsync    = C.MDB_MAPASYNC   // Flush asynchronously when using the WriteMap flag.
	NoTLS       = C.MDB_NOTLS      // Danger zone. When unset reader locktable slots are tied to their thread.
	NoLock      = C.MDB_NOLOCK     // Danger zone. LMDB does not use any locks.
	NoReadahead = C.MDB_NORDAHEAD  // Disable readahead. Requires OS support.
	NoMemInit   = C.MDB_NOMEMINIT  // Disable LMDB memory initialization.
)

// These flags are exclusively used in the Env.CopyFlags and Env.CopyFDFlags
// methods.
const (
	// Flags for Env.CopyFlags
	//
	// See mdb_env_copy2

	CopyCompact = C.MDB_CP_COMPACT // Perform compaction while copying
)

// DBI is a handle for a database in an Env.
//
// See MDB_dbi
type DBI C.MDB_dbi

// Env is opaque structure for a database environment.  A DB environment
// supports multiple databases, all residing in the same shared-memory map.
//
// See MDB_env.
type Env struct {
	_env *C.MDB_env

	// closeLock is used to allow the Txn finalizer to check if the Env has
	// been closed, so that it may know if it must abort.
	closeLock sync.RWMutex

	// for writers
	wkey *C.MDB_val
	wval *C.MDB_val

	// maximum total readers
	maxReaders int

	// rkeyMu and rkeyCond protects rkeyAvail and rkey
	rkeyMu   sync.Mutex
	rkeyCond *sync.Cond

	// available slots for readers are stored here
	rkeyAvail []int

	// keep a static pool of these, size maxReaders,
	// to avoid a C.malloc() allocation on each read.
	readSlots []*ReadSlot

	writeSlot *ReadSlot

	//readWorker []*sphynxReadWorker // size will be maxReaders
	readWorker *sphynxReadWorker // elastic sizing of goro pool possible?
}

type ReadSlot struct {
	slot     int // immutable once created
	skey     *C.MDB_val
	sval     *C.MDB_val
	mu       sync.Mutex // only one user at a time, and protect refCount/owner
	refCount int
	owner    int
}

func newReadSlot(i int) (rs *ReadSlot) {
	rs = &ReadSlot{
		slot: i,
		skey: (*C.MDB_val)(C.malloc(C.size_t(unsafe.Sizeof(C.MDB_val{})))),
		sval: (*C.MDB_val)(C.malloc(C.size_t(unsafe.Sizeof(C.MDB_val{})))),
	}
	vv("read slot %v created with skey=%p and sval=%p", i, rs.skey, rs.sval)
	return
}
func (rs *ReadSlot) free() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.owner != 0 {
		panic(fmt.Sprintf("should not be in ReadSlot.free() with slot still owned by gid=%v", rs.owner))
	}
	if rs.refCount != 0 {
		panic(fmt.Sprintf("should not be in ReadSlot.free() with slot having non-zero refCount = %v", rs.refCount))
	}
	C.free(unsafe.Pointer(rs.skey))
	rs.skey = nil
	C.free(unsafe.Pointer(rs.sval))
	rs.sval = nil
}

// GetOrWaitForReadSlot() returns when it has filled
// in the values of rs to be usable. ReturnReadSlot
// must be called with rs again when done reading.
func (env *Env) GetOrWaitForReadSlot() (rs *ReadSlot, err error) {
	env.rkeyMu.Lock()
	defer env.rkeyMu.Unlock()

	for len(env.rkeyAvail) == 0 {
		// Wait for a ReadSlot to become available.
		// We can block here, waiting forever if nobody else stops
		// reading. So make sure other read transactions finish,
		// and are as short as possible.
		env.rkeyCond.Wait()
	}
	i := env.rkeyAvail[0]
	env.rkeyAvail = env.rkeyAvail[1:]
	rs = env.readSlots[i]
	rs.mu.Lock()
	if rs.owner != 0 {
		vv("rs %p is still owned by gid %v", rs, rs.owner)
		panic(fmt.Sprintf("rs %p is still owned by gid %v", rs, rs.owner))
	}
	rs.refCount = 1
	rs.owner = curGID()
	vv("slot %v retreived from avail pool, now owned by gid=%v", i, rs.owner)
	rs.mu.Unlock()
	return
}

func (rs *ReadSlot) confirmOwned() {
	gid := curGID()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.owner != gid {
		panic(fmt.Sprintf("we(gid=%v) are not the owner of this ReadSlot %v p=%p, gid is: %v", gid, rs.slot, rs, rs.owner))
	}
}

// ReturnReadSlot must be called after the reader is
// done reading to return the resource to the memory
// pool. This avoids allocation, and defines a pool
// of readers whose size is defined by the NewEnv
// maxReaders parameter.
func (env *Env) ReturnReadSlot(rs *ReadSlot) {
	env.rkeyMu.Lock()
	rs.mu.Lock()

	rs.refCount--
	if rs.refCount == 0 {
		env.rkeyAvail = append(env.rkeyAvail, rs.slot)
		vv("returned to avail, slot %v  from gid=%v", rs.slot, rs.owner)

		rs.owner = 0 // not owned anymore

		// can't use defer because we want to signal unlocked,
		// to avoid spinning on Cond locks and missing the wake-up signal.
		rs.mu.Unlock()
		env.rkeyMu.Unlock()
		env.rkeyCond.Signal()
		return
	}
	rs.mu.Unlock()
	env.rkeyMu.Unlock()
}

// NewEnv allocates and initializes a new Env.
//
// It defaults to 256 concurrent readers max. If
// you want more or fewer, use NewEnvMaxReaders()
// and specify the limit. Readers over the limit
// will block in GetOrWaitForReadSlot until a
// slot becomes available.
//
// See mdb_env_create.
func NewEnv() (*Env, error) {
	return NewEnvMaxReaders(256)
}

func NewEnvMaxReaders(maxReaders int) (*Env, error) {
	env := &Env{
		wkey:       (*C.MDB_val)(C.malloc(C.size_t(unsafe.Sizeof(C.MDB_val{})))),
		wval:       (*C.MDB_val)(C.malloc(C.size_t(unsafe.Sizeof(C.MDB_val{})))),
		readSlots:  make([]*ReadSlot, maxReaders),
		writeSlot:  newReadSlot(-1),
		rkeyAvail:  make([]int, maxReaders, maxReaders),
		maxReaders: maxReaders,
		readWorker: newSphynxReadWorker(),
	}
	for i := 0; i < maxReaders; i++ {
		env.readSlots[i] = newReadSlot(i)
		env.rkeyAvail[i] = i
	}
	env.rkeyCond = sync.NewCond(&env.rkeyMu)

	ret := C.mdb_env_create(&env._env)
	if ret != success {
		return nil, operrno("mdb_env_create", ret)
	}

	runtime.SetFinalizer(env, (*Env).Close)
	return env, nil
}

// Open an environment handle. If this function fails Close() must be called to
// discard the Env handle.  Open passes flags|NoTLS to mdb_env_open.
//
// See mdb_env_open.
func (env *Env) Open(path string, flags uint, mode os.FileMode) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	ret := C.mdb_env_open(env._env, cpath, C.uint(NoTLS|flags), C.mdb_mode_t(mode))
	return operrno("mdb_env_open", ret)
}

var errNotOpen = errors.New("enivornment is not open")
var errNegSize = errors.New("negative size")

// FD returns the open file descriptor (or Windows file handle) for the given
// environment.  An error is returned if the environment has not been
// successfully Opened (where C API just retruns an invalid handle).
//
// See mdb_env_get_fd.
func (env *Env) FD() (uintptr, error) {
	// fdInvalid is the value -1 as a uintptr, which is used by LMDB in the
	// case that env has not been opened yet.  the strange construction is done
	// to avoid constant value overflow errors at compile time.
	const fdInvalid = ^uintptr(0)

	mf := new(C.mdb_filehandle_t)
	ret := C.mdb_env_get_fd(env._env, mf)
	err := operrno("mdb_env_get_fd", ret)
	if err != nil {
		return 0, err
	}
	fd := uintptr(*mf)

	if fd == fdInvalid {
		return 0, errNotOpen
	}
	return fd, nil
}

// ReaderList dumps the contents of the reader lock table as text.  Readers
// start on the second line as space-delimited fields described by the first
// line.
//
// See mdb_reader_list.
func (env *Env) ReaderList(fn func(string) error) error {
	ctx, done := newMsgFunc(fn)
	defer done()
	if fn == nil {
		ctx = 0
	}

	ret := C.lmdbgo_mdb_reader_list(env._env, C.size_t(ctx))
	if ret >= 0 {
		return nil
	}
	if ret < 0 && ctx != 0 {
		err := ctx.get().err
		if err != nil {
			return err
		}
	}
	return operrno("mdb_reader_list", ret)
}

// ReaderCheck clears stale entries from the reader lock table and returns the
// number of entries cleared.
//
// See mdb_reader_check()
func (env *Env) ReaderCheck() (int, error) {
	var _dead C.int
	ret := C.mdb_reader_check(env._env, &_dead)
	return int(_dead), operrno("mdb_reader_check", ret)
}

func (env *Env) close() bool {
	env.closeLock.Lock()
	if env._env == nil {
		env.closeLock.Unlock()
		return false
	}

	C.mdb_env_close(env._env)
	env._env = nil
	env.closeLock.Unlock()

	env.writeSlot.free()

	env.readWorker.halt.ReqStop.Close()
	<-env.readWorker.halt.Done.Chan

	for i := 0; i < env.maxReaders; i++ {
		env.readSlots[i].free()
	}

	return true
}

// Close shuts down the environment, releases the memory map, and clears the
// finalizer on env.
//
// See mdb_env_close.
func (env *Env) Close() error {
	if env.close() {
		runtime.SetFinalizer(env, nil)
		return nil
	}
	return errors.New("environment is already closed")
}

// CopyFD copies env to the the file descriptor fd.
//
// See mdb_env_copyfd.
func (env *Env) CopyFD(fd uintptr) error {
	ret := C.mdb_env_copyfd(env._env, C.mdb_filehandle_t(fd))
	return operrno("mdb_env_copyfd", ret)
}

// CopyFDFlag copies env to the file descriptor fd, with options.
//
// See mdb_env_copyfd2.
func (env *Env) CopyFDFlag(fd uintptr, flags uint) error {
	ret := C.mdb_env_copyfd2(env._env, C.mdb_filehandle_t(fd), C.uint(flags))
	return operrno("mdb_env_copyfd2", ret)
}

// Copy copies the data in env to an environment at path.
//
// See mdb_env_copy.
func (env *Env) Copy(path string) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	ret := C.mdb_env_copy(env._env, cpath)
	return operrno("mdb_env_copy", ret)
}

// CopyFlag copies the data in env to an environment at path created with flags.
//
// See mdb_env_copy2.
func (env *Env) CopyFlag(path string, flags uint) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	ret := C.mdb_env_copy2(env._env, cpath, C.uint(flags))
	return operrno("mdb_env_copy2", ret)
}

// Stat contains database status information.
//
// See MDB_stat.
type Stat struct {
	PSize         uint   // Size of a database page. This is currently the same for all databases.
	Depth         uint   // Depth (height) of the B-tree
	BranchPages   uint64 // Number of internal (non-leaf) pages
	LeafPages     uint64 // Number of leaf pages
	OverflowPages uint64 // Number of overflow pages
	Entries       uint64 // Number of data items
}

// Stat returns statistics about the environment.
//
// See mdb_env_stat.
func (env *Env) Stat() (*Stat, error) {
	var _stat C.MDB_stat
	ret := C.mdb_env_stat(env._env, &_stat)
	if ret != success {
		return nil, operrno("mdb_env_stat", ret)
	}
	stat := Stat{PSize: uint(_stat.ms_psize),
		Depth:         uint(_stat.ms_depth),
		BranchPages:   uint64(_stat.ms_branch_pages),
		LeafPages:     uint64(_stat.ms_leaf_pages),
		OverflowPages: uint64(_stat.ms_overflow_pages),
		Entries:       uint64(_stat.ms_entries)}
	return &stat, nil
}

// EnvInfo contains information an environment.
//
// See MDB_envinfo.
type EnvInfo struct {
	MapSize    int64 // Size of the data memory map
	LastPNO    int64 // ID of the last used page
	LastTxnID  int64 // ID of the last committed transaction
	MaxReaders uint  // maximum number of threads for the environment
	NumReaders uint  // maximum number of threads used in the environment
}

// Info returns information about the environment.
//
// See mdb_env_info.
func (env *Env) Info() (*EnvInfo, error) {
	var _info C.MDB_envinfo
	ret := C.mdb_env_info(env._env, &_info)
	if ret != success {
		return nil, operrno("mdb_env_info", ret)
	}
	info := EnvInfo{
		MapSize:    int64(_info.me_mapsize),
		LastPNO:    int64(_info.me_last_pgno),
		LastTxnID:  int64(_info.me_last_txnid),
		MaxReaders: uint(_info.me_maxreaders),
		NumReaders: uint(_info.me_numreaders),
	}
	return &info, nil
}

// Sync flushes buffers to disk.  If force is true a synchronous flush occurs
// and ignores any NoSync or MapAsync flag on the environment.
//
// See mdb_env_sync.
func (env *Env) Sync(force bool) error {
	ret := C.mdb_env_sync(env._env, cbool(force))
	return operrno("mdb_env_sync", ret)
}

// SetFlags sets flags in the environment.
//
// See mdb_env_set_flags.
func (env *Env) SetFlags(flags uint) error {
	ret := C.mdb_env_set_flags(env._env, C.uint(flags), C.int(1))
	return operrno("mdb_env_set_flags", ret)
}

// UnsetFlags clears flags in the environment.
//
// See mdb_env_set_flags.
func (env *Env) UnsetFlags(flags uint) error {
	ret := C.mdb_env_set_flags(env._env, C.uint(flags), C.int(0))
	return operrno("mdb_env_set_flags", ret)
}

// Flags returns the flags set in the environment.
//
// See mdb_env_get_flags.
func (env *Env) Flags() (uint, error) {
	var _flags C.uint
	ret := C.mdb_env_get_flags(env._env, &_flags)
	if ret != success {
		return 0, operrno("mdb_env_get_flags", ret)
	}
	return uint(_flags), nil
}

// Path returns the path argument passed to Open.  Path returns a non-nil error
// if env.Open() was not previously called.
//
// See mdb_env_get_path.
func (env *Env) Path() (string, error) {
	var cpath *C.char
	ret := C.mdb_env_get_path(env._env, &cpath)
	if ret != success {
		return "", operrno("mdb_env_get_path", ret)
	}
	if cpath == nil {
		return "", errNotOpen
	}
	return C.GoString(cpath), nil
}

// SetMapSize sets the size of the environment memory map.
//
// See mdb_env_set_mapsize.
func (env *Env) SetMapSize(size int64) error {
	if size < 0 {
		return errNegSize
	}
	ret := C.mdb_env_set_mapsize(env._env, C.size_t(size))
	return operrno("mdb_env_set_mapsize", ret)
}

// SetMaxReaders sets the maximum number of reader slots in the environment.
//
// See mdb_env_set_maxreaders.
func (env *Env) SetMaxReaders(size int) error {
	if size < 0 {
		return errNegSize
	}
	ret := C.mdb_env_set_maxreaders(env._env, C.uint(size))
	return operrno("mdb_env_set_maxreaders", ret)
}

// MaxReaders returns the maximum number of reader slots for the environment.
//
// See mdb_env_get_maxreaders.
func (env *Env) MaxReaders() (int, error) {
	var max C.uint
	ret := C.mdb_env_get_maxreaders(env._env, &max)
	return int(max), operrno("mdb_env_get_maxreaders", ret)
}

// MaxKeySize returns the maximum allowed length for a key.
//
// See mdb_env_get_maxkeysize.
func (env *Env) MaxKeySize() int {
	if env == nil {
		return int(C.mdb_env_get_maxkeysize(nil))
	}
	return int(C.mdb_env_get_maxkeysize(env._env))
}

// SetMaxDBs sets the maximum number of named databases for the environment.
//
// See mdb_env_set_maxdbs.
func (env *Env) SetMaxDBs(size int) error {
	if size < 0 {
		return errNegSize
	}
	ret := C.mdb_env_set_maxdbs(env._env, C.MDB_dbi(size))
	return operrno("mdb_env_set_maxdbs", ret)
}

// BeginTxn is an unsafe, low-level method to initialize a new transaction on
// env.  The Txn returned by BeginTxn is unmanaged and must be terminated by
// calling either its Abort or Commit methods to ensure that its resources are
// released.
//
// BeginTxn does not call runtime.LockOSThread.  Unless the Readonly flag is
// passed goroutines must call runtime.LockOSThread before calling BeginTxn and
// the returned Txn must not have its methods called from another goroutine.
// Failure to meet these restrictions can have undefined results that may
// include deadlocking your application.
//
// Instead of calling BeginTxn users should prefer calling the View and Update
// methods, which assist in management of Txn objects and provide OS thread
// locking required for write transactions.
//
// A finalizer detects unreachable, live transactions and logs thems to
// standard error.  The transactions are aborted, but their presence should be
// interpreted as an application error which should be patched so transactions
// are terminated explicitly.  Unterminated transactions can adversly effect
// database performance and cause the database to grow until the map is full.
//
// See mdb_txn_begin.
func (env *Env) BeginTxn(parent *Txn, flags uint) (*Txn, error) {
	txn, err := beginTxn(env, parent, flags)
	if txn != nil {
		runtime.SetFinalizer(txn, func(v interface{}) { v.(*Txn).finalize() })
	}
	return txn, err
}

func (env *Env) BeginTxnWithReadSlot(parent *Txn, flags uint, rs *ReadSlot) (*Txn, error) {
	txn, err := beginTxnWithReadSlot(env, parent, flags, rs)
	if txn != nil {
		runtime.SetFinalizer(txn, func(v interface{}) { v.(*Txn).finalize() })
	}
	return txn, err
}

func (env *Env) NewReadTxn() (*Txn, error) {
	return env.BeginTxn(nil, Readonly)
}

func (env *Env) NewRawReadTxn() (tx *Txn, err error) {
	tx, err = env.BeginTxn(nil, Readonly)
	if tx != nil {
		tx.RawRead = true
	}
	return
}

func (env *Env) NewWriteTxn() (*Txn, error) {
	return env.BeginTxn(nil, 0)
}

// RunTxn creates a new Txn and calls fn with it as an argument.  Run commits
// the transaction if fn returns nil otherwise the transaction is aborted.
// Because RunTxn terminates the transaction goroutines should not retain
// references to it or its data after fn returns.
//
// RunTxn does not call runtime.LockOSThread.  Unless the Readonly flag is
// passed the calling goroutine should ensure it is locked to its thread and
// any goroutines started by fn must not call methods on the Txn object it is
// passed.
//
// See mdb_txn_begin.
func (env *Env) RunTxn(flags uint, fn TxnOp) error {
	return env.run(false, flags, fn)
}

// View creates a readonly transaction with a consistent view of the
// environment and passes it to fn.  View terminates its transaction after fn
// returns.  Any error encountered by View is returned.
//
// Unlike with Update transactions, goroutines created by fn are free to call
// methods on the Txn passed to fn provided they are synchronized in their
// accesses (e.g. using a mutex or channel).
//
// Any call to Commit, Abort, Reset or Renew on a Txn created by View will
// panic.
func (env *Env) View(fn TxnOp) error {
	return env.run(false, Readonly, fn)
}

// Update calls fn with a writable transaction.  Update commits the transaction
// if fn returns a nil error otherwise Update aborts the transaction and
// returns the error.
//
// Update calls runtime.LockOSThread to lock the calling goroutine to its
// thread and until fn returns and the transaction has been terminated, at
// which point runtime.UnlockOSThread is called.  If the calling goroutine is
// already known to be locked to a thread, use UpdateLocked instead to avoid
// premature unlocking of the goroutine.
//
// Neither Update nor UpdateLocked cannot be called safely from a goroutine
// where it isn't known if runtime.LockOSThread has been called.  In such
// situations writes must either be done in a newly created goroutine which can
// be safely locked, or through a worker goroutine that accepts updates to
// apply and delivers transaction results using channels.  See the package
// documentation and examples for more details.
//
// Goroutines created by the operation fn must not use methods on the Txn
// object that fn is passed.  Doing so would have undefined and unpredictable
// results for your program (likely including data loss, deadlock, etc).
//
// Any call to Commit, Abort, Reset or Renew on a Txn created by Update will
// panic.
func (env *Env) Update(fn TxnOp) error {
	return env.run(true, 0, fn)
}

// UpdateLocked behaves like Update but does not lock the calling goroutine to
// its thread.  UpdateLocked should be used if the calling goroutine is already
// locked to its thread for another purpose.
//
// Neither Update nor UpdateLocked cannot be called safely from a goroutine
// where it isn't known if runtime.LockOSThread has been called.  In such
// situations writes must either be done in a newly created goroutine which can
// be safely locked, or through a worker goroutine that accepts updates to
// apply and delivers transaction results using channels.  See the package
// documentation and examples for more details.
//
// Goroutines created by the operation fn must not use methods on the Txn
// object that fn is passed.  Doing so would have undefined and unpredictable
// results for your program (likely including data loss, deadlock, etc).
//
// Any call to Commit, Abort, Reset or Renew on a Txn created by UpdateLocked
// will panic.
func (env *Env) UpdateLocked(fn TxnOp) error {
	return env.run(false, 0, fn)
}

func (env *Env) run(lock bool, flags uint, fn TxnOp) error {
	if lock {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	txn, err := beginTxn(env, nil, flags)
	if err != nil {
		return err
	}
	return txn.runOpTerm(fn)
}

// CloseDBI closes the database handle, db.  Normally calling CloseDBI
// explicitly is not necessary.
//
// It is the caller's responsibility to serialize calls to CloseDBI.
//
// See mdb_dbi_close.
func (env *Env) CloseDBI(db DBI) {
	C.mdb_dbi_close(env._env, C.MDB_dbi(db))
}

type SphynxReadFunc func(txn *Txn, readslot int) (err error)

// job cannot actually know about the txn, since that
// must stay local to the goroutine(thread) that created it.
type sphynxReadJob struct {
	f        SphynxReadFunc
	done     chan struct{}
	err      error
	flags    uint
	readSlot *ReadSlot
	env      *Env
}

func (env *Env) newSphynxReadJob(f SphynxReadFunc) (j *sphynxReadJob) {
	j = &sphynxReadJob{
		env:   env,
		f:     f,
		flags: Readonly,
		done:  make(chan struct{}),
	}
	return
}

// SphynxReader runs srf on a background goroutine
// that has been tied to an OS thread as LMDB needs,
// and has obtained a transaction on that thread.
// This last is also an LMDB requirement. Transactions
// cannot be used on different threads than they
// were created on. Env creates these threads and
// binds goroutines to them at NewEnv creation time,
// based on the count of maxReaders.
//
// The Lion(txn) + Eagle(goroutine) = Sphynx, a hybrid creature.
//
// The riddle is still a mystery.
func (env *Env) SphynxReader(srf SphynxReadFunc) (err error) {
	job := env.newSphynxReadJob(srf)

	// block until we can get a read slot
	job.readSlot, err = env.GetOrWaitForReadSlot()
	panicOn(err)

	//env.readWorker[job.readSlot.slot].jobsCh <- job
	env.readWorker.jobsCh <- job
	<-job.done
	return job.err
}

type sphynxReadWorker struct {
	jobsCh chan *sphynxReadJob
	halt   *idem.Halter

	childGoro []*idem.Halter
}

func newSphynxReadWorker() *sphynxReadWorker {
	w := &sphynxReadWorker{
		jobsCh: make(chan *sphynxReadJob),
		halt:   idem.NewHalter(),
	}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		defer w.halt.Done.Close()
		for {
			select {
			case <-w.halt.ReqStop.Chan:
				vv("read worker sees shutdown request, waiting on any child goro")
				for _, h := range w.childGoro {
					h.ReqStop.Close()
					<-h.Done.Chan
				}
				return
			case job := <-w.jobsCh:
				hlt := idem.NewHalter()
				w.childGoro = append(w.childGoro, hlt)
				go func(hlt *idem.Halter) {
					runtime.LockOSThread()
					defer runtime.UnlockOSThread()
					defer close(job.done)
					defer hlt.Done.Close()

					gid := curGID()
					job.readSlot.mu.Lock()
					slot := job.readSlot.slot
					vv("worker goro gid %v taking ownership of slot %v from prior owner %v", gid, slot, job.readSlot.owner)
					job.readSlot.owner = gid
					job.readSlot.mu.Unlock()

					vv("about beginTxn with slot %v from job", slot)
					defer vv("defer firing, done with slot %v from job", slot) // never seen

					txn, err := beginTxnWithReadSlot(job.env, nil, job.flags, job.readSlot)
					panicOn(err)
					vv("called beginTxnWithReadSlot(slot %v) on gid=%v", slot, gid)

					// run the read-only txn code on this safely locked
					// to thread goroutine that allocated the txn.
					job.err = job.f(txn, txn.readSlot.slot)
					vv("done running function on slot %v gid=%v", slot, gid) // never reached

					// have to do this while still on this goroutine.
					txn.Abort() // cleanup reader

					vv("done running function on slot %v", slot)
				}(hlt)
			}
		}
	}()
	return w
}
