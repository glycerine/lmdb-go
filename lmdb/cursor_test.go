package lmdb

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestCursor_Txn(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var db DBI
	err := env.Update(func(txn *Txn) (err error) {
		db, err = txn.OpenRoot(0)
		if err != nil {
			return err
		}
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}

		_txn := cur.Txn()
		if _txn == nil {
			t.Errorf("nil cursor txn")
		}

		cur.Close()

		_txn = cur.Txn()
		if _txn != nil {
			t.Errorf("non-nil cursor txn")
		}

		return err
	})
	if err != nil {
		t.Error(err)
		return
	}
}

func TestCursor_DBI(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	err := env.Update(func(txn *Txn) (err error) {
		db, err := txn.OpenDBI("db", Create)
		if err != nil {
			return err
		}
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		dbcur := cur.DBI()
		if dbcur != db {
			cur.Close()
			return fmt.Errorf("unequal db: %v != %v", dbcur, db)
		}
		cur.Close()
		dbcur = cur.DBI()
		if dbcur == db {
			return fmt.Errorf("db: %v", dbcur)
		}
		if dbcur != ^DBI(0) {
			return fmt.Errorf("db: %v", dbcur)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func TestCursor_Close(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	txn, err := env.BeginTxn(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Abort()

	db, err := txn.OpenDBI("testing", Create)
	if err != nil {
		t.Fatal(err)
	}

	cur, err := txn.OpenCursor(db)
	if err != nil {
		t.Fatal(err)
	}
	cur.Close()
	cur.Close()
	err = cur.Put([]byte("closedput"), []byte("shouldfail"), 0)
	if err == nil {
		t.Fatalf("expected error: put on closed cursor")
	}
}

func TestCursor_bytesBuffer(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	db, err := openRoot(env, 0)
	if err != nil {
		t.Error(err)
		return
	}

	err = env.Update(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		defer cur.Close()
		k := new(bytes.Buffer)
		k.WriteString("hello")
		v := new(bytes.Buffer)
		v.WriteString("world")
		return cur.Put(k.Bytes(), v.Bytes(), 0)
	})
	if err != nil {
		t.Error(err)
		return
	}

	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		defer cur.Close()
		k := new(bytes.Buffer)
		k.WriteString("hello")
		_k, v, err := cur.Get(k.Bytes(), nil, SetKey)
		if err != nil {
			return err
		}
		if !bytes.Equal(_k, k.Bytes()) {
			return fmt.Errorf("unexpected key: %q", _k)
		}
		if !bytes.Equal(v, []byte("world")) {
			return fmt.Errorf("unexpected value: %q", v)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
		return
	}
}

func TestCursor_PutReserve(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var db DBI
	key := "reservekey"
	val := "reserveval"
	err := env.Update(func(txn *Txn) (err error) {
		db, err = txn.CreateDBI("testing")
		if err != nil {
			return err
		}

		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		defer cur.Close()

		p, err := cur.PutReserve([]byte(key), len(val), 0)
		if err != nil {
			return err
		}
		copy(p, val)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = env.View(func(txn *Txn) (err error) {
		dbval, err := txn.Get(db, []byte(key))
		if err != nil {
			return err
		}
		if !bytes.Equal(dbval, []byte(val)) {
			return fmt.Errorf("unexpected val %q != %q", dbval, val)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCursor_Get_KV(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var dbi DBI
	err := env.Update(func(txn *Txn) (err error) {
		dbi, err = txn.OpenDBI("testdb", Create|DupSort)
		return err
	})
	if err != nil {
		t.Errorf("%s", err)
		return
	}

	err = env.Update(func(txn *Txn) (err error) {
		put := func(k, v []byte) {
			if err == nil {
				err = txn.Put(dbi, k, v, 0)
			}
		}
		put([]byte("key"), []byte("1"))
		put([]byte("key"), []byte("2"))
		put([]byte("key"), []byte("3"))
		return err
	})
	if err != nil {
		t.Errorf("%s", err)
	}

	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		k, v, err := cur.Get([]byte("key"), []byte("0"), GetBothRange)
		if err != nil {
			return err
		}
		if string(k) != "key" {
			t.Errorf("unexpected key: %q (not %q)", k, "key")
		}
		if string(v) != "1" {
			t.Errorf("unexpected value: %q (not %q)", k, "1")
		}

		_, _, err = cur.Get([]byte("key"), []byte("1"), GetBoth)
		return err
	})
	if err != nil {
		t.Errorf("%s", err)
	}
}

func TestCursor_Get_op_Set_bytesBuffer(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var dbi DBI
	err := env.Update(func(txn *Txn) (err error) {
		dbi, err = txn.OpenDBI("testdb", Create|DupSort)
		return err
	})
	if err != nil {
		t.Errorf("%s", err)
		return
	}

	err = env.Update(func(txn *Txn) (err error) {
		put := func(k, v []byte) {
			if err == nil {
				err = txn.Put(dbi, k, v, 0)
			}
		}
		put([]byte("k1"), []byte("v11"))
		put([]byte("k1"), []byte("v12"))
		put([]byte("k1"), []byte("v13"))
		put([]byte("k2"), []byte("v21"))
		put([]byte("k2"), []byte("v22"))
		put([]byte("k2"), []byte("v23"))
		return err
	})
	if err != nil {
		t.Errorf("%s", err)
	}

	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		// Create bytes.Buffer values containing a amount of bytes.  Byte
		// slices returned from buf.Bytes() have a history of tricking the cgo
		// argument checker.
		var kbuf bytes.Buffer
		kbuf.WriteString("k2")

		k, _, err := cur.Get(kbuf.Bytes(), nil, Set)
		if err != nil {
			return err
		}
		if string(k) != kbuf.String() {
			t.Errorf("unexpected key: %q (not %q)", k, kbuf.String())
		}

		// No guarantee is made about the return value of mdb_cursor_get when
		// MDB_SET is the op, so its value is not checked as part of this test.
		// That said, it is important that Cursor.Get not panic if given a
		// short buffer as an input value for a Set op (despite that not really
		// having any significance)
		var vbuf bytes.Buffer
		vbuf.WriteString("v22")

		k, _, err = cur.Get(kbuf.Bytes(), vbuf.Bytes(), Set)
		if err != nil {
			return err
		}
		if string(k) != kbuf.String() {
			t.Errorf("unexpected key: %q (not %q)", k, kbuf.String())
		}

		return nil
	})
	if err != nil {
		t.Errorf("%s", err)
	}
}

func TestCursor_Get_DupFixed(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	const datasize = 16
	pagesize := os.Getpagesize()
	numitems := (2 * pagesize / datasize) + 1

	var dbi DBI
	key := []byte("key")
	err := env.Update(func(txn *Txn) (err error) {
		dbi, err = txn.OpenRoot(DupSort | DupFixed)
		if err != nil {
			return err
		}

		for i := int64(0); i < int64(numitems); i++ {
			err = txn.Put(dbi, key, []byte(fmt.Sprintf("%016x", i)), 0)
		}

		return nil
	})
	if err != nil {
		t.Error(err)
	}

	var items [][]byte
	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		for {
			k, first, err := cur.Get(nil, nil, NextNoDup)
			if IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}

			if string(k) != string(key) {
				return fmt.Errorf("key: %s", k)
			}

			stride := len(first)

			for {
				_, v, err := cur.Get(nil, nil, NextMultiple)
				if IsNotFound(err) {
					break
				}
				if err != nil {
					return err
				}

				multi := WrapMulti(v, stride)
				for i := 0; i < multi.Len(); i++ {
					items = append(items, multi.Val(i))
				}
			}
		}
	})
	if err != nil {
		t.Error(err)
	}

	if len(items) != numitems {
		t.Errorf("unexpected number of items: %d (!= %d)", len(items), numitems)
	}

	for i, b := range items {
		expect := fmt.Sprintf("%016x", i)
		if string(b) != expect {
			t.Errorf("unexpected value: %q (!= %q)", b, expect)
		}
	}
}

func TestCursor_Get_reverse(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var dbi DBI
	err := env.Update(func(txn *Txn) (err error) {
		dbi, err = txn.OpenRoot(0)
		if err != nil {
			return err
		}
		err = txn.Put(dbi, []byte("k0"), []byte("v0"), 0)
		if err != nil {
			return err
		}
		err = txn.Put(dbi, []byte("k1"), []byte("v1"), 0)
		if err != nil {
			return err
		}
		return err
	})
	if err != nil {
		t.Error(err)
	}

	type Item struct{ k, v []byte }
	var items []Item

	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		for {
			k, v, err := cur.Get(nil, nil, Prev)
			if IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			items = append(items, Item{k, v})
		}
	})
	if err != nil {
		t.Error(err)
	}

	expect := []Item{
		{[]byte("k1"), []byte("v1")},
		{[]byte("k0"), []byte("v0")},
	}
	if !reflect.DeepEqual(items, expect) {
		t.Errorf("unexpected items %q (!= %q)", items, expect)
	}
}

func TestCursor_PutMulti(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	key := []byte("k")
	items := [][]byte{
		[]byte("v0"),
		[]byte("v2"),
		[]byte("v1"),
	}
	page := bytes.Join(items, nil)
	stride := 2

	var dbi DBI
	err := env.Update(func(txn *Txn) (err error) {
		dbi, err = txn.OpenRoot(Create | DupSort | DupFixed)
		if err != nil {
			return err
		}

		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		return cur.PutMulti(key, page, stride, 0)
	})
	if err != nil {
		t.Error(err)
	}

	expect := [][]byte{
		[]byte("v0"),
		[]byte("v1"),
		[]byte("v2"),
	}
	var dbitems [][]byte
	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()

		for {
			k, v, err := cur.Get(nil, nil, Next)
			if IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if string(k) != "k" {
				return fmt.Errorf("key: %q", k)
			}
			dbitems = append(dbitems, v)
		}
	})
	if err != nil {
		t.Error(err)
	}
	if !reflect.DeepEqual(dbitems, expect) {
		t.Errorf("unexpected items: %q (!= %q)", dbitems, items)
	}
}

func TestCursor_Del(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var db DBI
	type Item struct{ k, v string }
	items := []Item{
		{"k0", "k0"},
		{"k1", "k1"},
		{"k2", "k2"},
	}
	err := env.Update(func(txn *Txn) (err error) {
		db, err = txn.CreateDBI("testing")
		if err != nil {
			return err
		}

		for _, item := range items {
			err := txn.Put(db, []byte(item.k), []byte(item.v), 0)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Error(err)
	}

	err = env.Update(func(txn *Txn) (err error) {
		txn.RawRead = true
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}

		item := items[1]
		k, v, err := cur.Get([]byte(item.k), nil, SetKey)
		if err != nil {
			return err
		}
		if !bytes.Equal(k, []byte(item.k)) {
			return fmt.Errorf("found key %q (!= %q)", k, item.k)
		}
		if !bytes.Equal(v, []byte(item.v)) {
			return fmt.Errorf("found value %q (!= %q)", k, item.v)
		}

		err = cur.Del(0)
		if err != nil {
			return err
		}

		k, v, err = cur.Get(nil, nil, Next)
		if err != nil {
			return fmt.Errorf("post-delete: %v", err)
		}
		item = items[2]
		if err != nil {
			return err
		}
		if !bytes.Equal(k, []byte(item.k)) {
			return fmt.Errorf("found key %q (!= %q)", k, item.k)
		}
		if !bytes.Equal(v, []byte(item.v)) {
			return fmt.Errorf("found value %q (!= %q)", k, item.v)
		}

		return nil
	})
	if err != nil {
		t.Error(err)
	}

	var newitems []Item
	err = env.View(func(txn *Txn) (err error) {
		txn.RawRead = true
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		next := func(cur *Cursor) (k, v []byte, err error) { return cur.Get(nil, nil, Next) }
		for k, v, err := next(cur); !IsNotFound(err); k, v, err = next(cur) {
			if err != nil {
				return err
			}
			newitems = append(newitems, Item{string(k), string(v)})
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}

	expectitems := []Item{
		items[0],
		items[2],
	}
	if !reflect.DeepEqual(newitems, expectitems) {
		t.Errorf("unexpected items %q (!= %q)", newitems, expectitems)
	}
}

// This test verifies the behavior of Cursor.Count when DupSort is provided.
func TestCursor_Count_DupSort(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var db DBI
	err := env.Update(func(txn *Txn) (err error) {
		db, err = txn.OpenDBI("testingdup", Create|DupSort)
		if err != nil {
			return err
		}

		put := func(k, v string) {
			if err != nil {
				return
			}
			err = txn.Put(db, []byte(k), []byte(v), 0)
		}
		put("k", "v0")
		put("k", "v1")

		return err
	})
	if err != nil {
		t.Error(err)
	}

	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		defer cur.Close()

		_, _, err = cur.Get(nil, nil, First)
		if err != nil {
			return err
		}
		numdup, err := cur.Count()
		if err != nil {
			return err
		}

		if numdup != 2 {
			t.Errorf("unexpected count: %d != %d", numdup, 2)
		}

		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

// This test verifies the behavior of Cursor.Count when DupSort is not enabled
// on the database.
func TestCursor_Count_noDupSort(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var db DBI
	err := env.Update(func(txn *Txn) (err error) {
		db, err = txn.OpenDBI("testingnodup", Create)
		if err != nil {
			return err
		}

		return txn.Put(db, []byte("k"), []byte("v1"), 0)
	})
	if err != nil {
		t.Error(err)
	}

	// it is an error to call Count if the underlying database does not allow
	// duplicate keys.
	err = env.View(func(txn *Txn) (err error) {
		cur, err := txn.OpenCursor(db)
		if err != nil {
			return err
		}
		defer cur.Close()

		_, _, err = cur.Get(nil, nil, First)
		if err != nil {
			return err
		}
		_, err = cur.Count()
		if err == nil {
			t.Error("expected error")
			return nil
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func TestCursor_Renew(t *testing.T) {
	env := setup(t)
	defer clean(env, t)

	var db DBI
	err := env.Update(func(txn *Txn) (err error) {
		db, err = txn.OpenRoot(0)
		return err
	})
	if err != nil {
		t.Error(err)
		return
	}

	err = env.Update(func(txn *Txn) (err error) {
		put := func(k, v string) {
			if err == nil {
				err = txn.Put(db, []byte(k), []byte(v), 0)
			}
		}
		put("k1", "v1")
		put("k2", "v2")
		put("k3", "v3")
		return err
	})
	if err != nil {
		t.Error("err")
	}

	var cur *Cursor
	err = env.View(func(txn *Txn) (err error) {
		cur, err = txn.OpenCursor(db)
		if err != nil {
			return err
		}

		k, v, err := cur.Get(nil, nil, Next)
		if err != nil {
			return err
		}
		if string(k) != "k1" {
			return fmt.Errorf("key: %q", k)
		}
		if string(v) != "v1" {
			return fmt.Errorf("val: %q", v)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}

	err = env.View(func(txn *Txn) (err error) {
		err = cur.Renew(txn)
		if err != nil {
			return err
		}

		k, v, err := cur.Get(nil, nil, Next)
		if err != nil {
			return err
		}
		if string(k) != "k1" {
			return fmt.Errorf("key: %q", k)
		}
		if string(v) != "v1" {
			return fmt.Errorf("val: %q", v)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func BenchmarkCursor(b *testing.B) {
	env := setup(b)
	defer clean(env, b)

	var db DBI
	err := env.View(func(txn *Txn) (err error) {
		db, err = txn.OpenRoot(0)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		b.Error(err)
		return
	}

	err = env.View(func(txn *Txn) (err error) {
		b.ResetTimer()
		defer b.StopTimer()

		for i := 0; i < b.N; i++ {
			cur, err := txn.OpenCursor(db)
			if err != nil {
				return err
			}
			cur.Close()
		}
		return
	})
	if err != nil {
		b.Error(err)
		return
	}
}

func BenchmarkCursor_Renew(b *testing.B) {
	env := setup(b)
	defer clean(env, b)

	var cur *Cursor
	err := env.View(func(txn *Txn) (err error) {
		db, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		cur, err = txn.OpenCursor(db)
		return err
	})
	if err != nil {
		b.Error(err)
		return
	}

	env.View(func(txn *Txn) (err error) {
		b.ResetTimer()
		defer b.StopTimer()

		for i := 0; i < b.N; i++ {
			err = cur.Renew(txn)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func TestConcurrentReadingAndWriting(t *testing.T) {
	if !testing.Short() {
		t.Skip("ironically skipping an infinitely running test when not in short mode")
	}

	rand.Seed(1)
	maxr := 9
	env, err := NewEnvMaxReaders(maxr)
	if err != nil {
		t.Fatalf("env: %s", err)
	}
	path, err := ioutil.TempDir("", "mdb_test")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	vv("temp dir '%v'", path)
	err = os.MkdirAll(path, 0770)
	if err != nil {
		t.Fatalf("mkdir: %s", path)
	}
	err = env.SetMaxDBs(64 << 10)
	if err != nil {
		t.Fatalf("setmaxdbs: %v", err)
	}
	var myflags uint
	err = env.Open(path, myflags, 0664)
	if err != nil {
		t.Fatalf("open: %s", err)
	}

	// cleanup
	defer func() {
		path, err := env.Path()
		if err != nil {
			t.Errorf("path: %v", err)
		}
		err = env.Close()
		if err != nil {
			t.Errorf("close: %s", err)
		}
		if path != "" {
			err = os.RemoveAll(path)
			if err != nil {
				t.Errorf("remove: %v", err)
			}
		}
	}()

	var dbi DBI
	err = env.Update(func(txn *Txn) (err error) {
		dbi, err = txn.OpenDBI("testdb", Create)
		return err
	})
	panicOn(err)

	err = env.Update(func(txn *Txn) (err error) {
		put := func(k, v []byte) {
			if err == nil {
				err = txn.Put(dbi, k, v, 0)
			}
		}
		put([]byte{0}, []byte("v0"))
		put([]byte("1big_huge_ginormous_key"), []byte("v1"))
		put([]byte("2"), []byte("v2"))
		put([]byte("3"), []byte("v3"))
		put([]byte("4"), []byte("v4"))
		put([]byte("5"), []byte("v5"))
		return err
	})
	if err != nil {
		t.Errorf("%s", err)
	}

	// start goroutines that are constantly reading
	// from a cursor
	reader := func(readno int) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		for {
			txn, err := env.NewRawReadTxn()
			panicOn(err)
			vv("readerno %v has made a new txn", readno)

			cur, err := txn.OpenCursor(dbi)
			panicOn(err)

			for i := 0; i < 3e5; i++ {
				var k, v []byte
				var err error
				if i == 0 {
					// must give it at least a zero byte here to start.
					k, v, err = cur.Get([]byte{0}, nil, SetRange)
					panicOn(err)
				} else {
					k, v, err = cur.Get([]byte("hello"), nil, Next)
					if IsNotFound(err) {
						//vv("not found")
						// start over
						k, v, err = cur.Get([]byte{0}, nil, SetRange)
						panicOn(err)
					} else {
						panicOn(err)
					}
				}
				_, _ = k, v
				if i%1e5 == 0 {
					vv("reader %v at i=%v  sees key:'%v' -> val:'%v'", readno, i, string(k), string(v))
				}
			}
			cur.Close()
			txn.Abort()

			// pause for a little, to let another reader get started.
			pause := rand.Intn(500)
			time.Sleep(time.Millisecond * time.Duration(pause))
		} // endless for
	}

	// start a bunch of readers, staggered
	k := 0
	for j := 0; j < 4; j++ {
		go reader(k)
		k++
		time.Sleep(250 * time.Millisecond)
		go reader(k)
		k++
		time.Sleep(125 * time.Millisecond)
		go reader(k)
		k++
		time.Sleep(75 * time.Millisecond)
	}

	writer := func() {
		// add one and delete one
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		for {
			txn, err := env.NewWriteTxn()
			panicOn(err)
			vv("writer has made a new txn")

			for i := 0; i < 3e5; i++ {

				kx := rand.Intn(10)
				k := []byte(fmt.Sprintf("writers_k%v", kx))
				v := []byte(fmt.Sprintf("writers_v%v", kx))

				// 90/10 put or delete
				rnd := rand.Intn(10)
				switch rnd {
				default:
					err := txn.Put(dbi, k, v, 0)
					panicOn(err)
				case 1: // 10% of the time, delete
					err := txn.Del(dbi, k, nil)
					if IsNotFound(err) {
						// ok, ignore. will be frequent.
					} else {
						panicOn(err)
						if i%1e4 == 0 {
							vv("writer deleted k '%v' ok", string(k))
						}
					}
				}

				if i%1e5 == 0 {
					vv("writer at i=%v  sees key:'%v' -> val:'%v'", i, string(k), string(v))
				}
			}
			panicOn(txn.Commit())

			pause := rand.Intn(500)
			time.Sleep(time.Millisecond * time.Duration(pause))
		} // endless for
	}
	go writer()

	select {}
}

// when you make a new txn, you automatically
// get a good goroutine to run it on.
// lion(txn) + eagle(goroutine) = sphynx
// the riddle will be revealed shortly.
func TestSphynx(t *testing.T) {
	if !testing.Short() {
		t.Skip("ironically skipping an infinitely running test when not in short mode")
	}

	rand.Seed(1)
	maxr := 7
	env, err := NewEnvMaxReaders(maxr)
	if err != nil {
		t.Fatalf("env: %s", err)
	}
	env.UseSphynxReader()

	path, err := ioutil.TempDir("", "mdb_test")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	vv("temp dir '%v'", path)
	err = os.MkdirAll(path, 0770)
	if err != nil {
		t.Fatalf("mkdir: %s", path)
	}
	err = env.SetMaxDBs(64 << 10)
	if err != nil {
		t.Fatalf("setmaxdbs: %v", err)
	}
	var myflags uint
	err = env.Open(path, myflags, 0664)
	if err != nil {
		t.Fatalf("open: %s", err)
	}

	// cleanup
	defer func() {
		path, err := env.Path()
		if err != nil {
			t.Errorf("path: %v", err)
		}
		err = env.Close()
		if err != nil {
			t.Errorf("close: %s", err)
		}
		if path != "" {
			err = os.RemoveAll(path)
			if err != nil {
				t.Errorf("remove: %v", err)
			}
		}
	}()

	var dbi DBI
	err = env.Update(func(txn *Txn) (err error) {
		dbi, err = txn.OpenDBI("testdb", Create)
		return err
	})
	panicOn(err)

	err = env.Update(func(txn *Txn) (err error) {
		put := func(k, v []byte) {
			if err == nil {
				err = txn.Put(dbi, k, v, 0)
			}
		}
		put([]byte{0}, []byte("v0"))
		put([]byte("1big_huge_ginormous_key"), []byte("v1"))
		put([]byte("2"), []byte("v2"))
		put([]byte("3"), []byte("v3"))
		put([]byte("4"), []byte("v4"))
		put([]byte("5"), []byte("v5"))
		return err
	})
	if err != nil {
		t.Errorf("%s", err)
	}

	reader := func() {
		for {
			err := env.SphynxReader(func(txn *Txn, readslot int) (err error) {
				panicOn(err)
				vv("new Sphynx has readslot %v has made a new txn", readslot)

				cur, err := txn.OpenCursor(dbi)
				panicOn(err)

				for i := 0; i < 3e5; i++ {
					var k, v []byte
					var err error
					if i == 0 {
						// must give it at least a zero byte here to start.
						k, v, err = cur.Get([]byte{0}, nil, SetRange)
						panicOn(err)
					} else {
						k, v, err = cur.Get([]byte("hello"), nil, Next)
						if IsNotFound(err) {
							//vv("not found")
							// start over
							k, v, err = cur.Get([]byte{0}, nil, SetRange)
							panicOn(err)
						} else {
							panicOn(err)
						}
					}
					_, _ = k, v
					if i%1e4 == 0 {
						vv("reader %v at i=%v  sees key:'%v' -> val:'%v'", readslot, i, string(k), string(v))
					}
				}
				cur.Close()
				return nil
			})
			vv("SphynxReader returned err='%v'", err)
			pause := rand.Intn(500)
			time.Sleep(time.Millisecond * time.Duration(pause))

		} // endless for
	} // defn reader

	// start a bunch of readers, staggered
	for k := 0; k < 11; k++ {
		pause := rand.Intn(121)
		time.Sleep(time.Millisecond * time.Duration(pause))
		go reader()
	}

	writer := func() {
		// add one and delete one
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		for {
			txn, err := env.NewWriteTxn()
			panicOn(err)
			vv("writer has made a new txn")

			for i := 0; i < 3e5; i++ {

				kx := rand.Intn(10)
				k := []byte(fmt.Sprintf("writers_k%v", kx))
				v := []byte(fmt.Sprintf("writers_v%v", kx))

				// 90/10 put or delete
				rnd := rand.Intn(10)
				switch rnd {
				default:
					err := txn.Put(dbi, k, v, 0)
					panicOn(err)
				case 1: // 10% of the time, delete
					err := txn.Del(dbi, k, nil)
					if IsNotFound(err) {
						// ok, ignore. will be frequent.
					} else {
						panicOn(err)
						if i%1e4 == 0 {
							vv("writer deleted k '%v' ok", string(k))
						}
					}
				}

				if i%1e5 == 0 {
					vv("writer at i=%v  sees key:'%v' -> val:'%v'", i, string(k), string(v))
				}
			}
			panicOn(txn.Commit())

			pause := rand.Intn(500)
			time.Sleep(time.Millisecond * time.Duration(pause))
		} // endless for
	}
	go writer()

	select {}
}

// totally separate Env for each file, does it work?
func TestTwoDatabaseFilesOpenAtOnce(t *testing.T) {
	if !testing.Short() {
		t.Skip("ironically skipping an infinitely running test when not in short mode")
	}
	openDB := func(path string) (*Env, error) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		rand.Seed(1)
		maxr := 7
		env, err := NewEnvMaxReaders(maxr)
		panicOn(err)
		env.UseSphynxReader()
		//defer env.Close()

		env.SetMapSize(1 << 20) // default 1MB is too small, but catches our growth without bounds problem.
		//err = os.MkdirAll(path, 0770)

		err = env.SetMaxDBs(1)
		panicOn(err)

		var myflags uint = NoReadahead | NoSubdir
		err = env.Open(path, myflags, 0664)
		panicOn(err)

		// In any real application it is important to check for readers that were
		// never closed by their owning process, and for which the owning process
		// has exited.  See the documentation on transactions for more information.
		staleReaders, err := env.ReaderCheck()
		panicOn(err)
		if staleReaders > 0 {
			vv("cleared %d reader slots from dead processes", staleReaders)
		}

		// // cleanup
		// defer func() {
		// 	vv("cleanup running")
		// 	r := recover()
		// 	if r != nil {
		// 		vv("cleanup func running on panic: '%v'", r)
		// 		panic(r)
		// 	}
		// 	path, err := env.Path()
		// 	panicOn(err)
		// 	err = env.Close() // during cleanup, not all txn are finished!!
		// 	panicOn(err)

		// 	if path != "" {
		// 		err = os.RemoveAll(path)
		// 		panicOn(err)
		// 	}
		// }()

		var dbi DBI
		err = env.Update(func(txn *Txn) (err error) {
			dbi, err = txn.OpenDBI("testdb", Create)
			return err
		})
		panicOn(err)

		err = env.Update(func(txn *Txn) (err error) {
			put := func(k, v []byte) {
				if err == nil {
					err = txn.Put(dbi, k, v, 0)
				}
			}
			put([]byte{0}, []byte("v0"))
			put([]byte("1big_huge_ginormous_key"), []byte("v1"))
			put([]byte("2"), []byte("v2"))
			put([]byte("3"), []byte("v3"))
			put([]byte("4"), []byte("v4"))
			put([]byte("5"), []byte("v5"))
			return err
		})
		panicOn(err)

		barrier := NewBarrier()

		reader := func() {
			for {
				err := env.SphynxReader(func(txn *Txn, readslot int) (err error) {
					panicOn(err)
					//vv("new Sphynx has readslot %v has made a new txn, on gid=%v", readslot, curGID())

					cur, err := txn.OpenCursor(dbi)
					panicOn(err)

					for i := 0; i < 30; i++ {
						var k, v []byte
						var err error
						if i == 0 {
							// must give it at least a zero byte here to start.
							k, v, err = cur.Get([]byte{0}, nil, SetRange) // deadlock here
							panicOn(err)
						} else {
							k, v, err = cur.Get([]byte("hello"), nil, Next)
							if IsNotFound(err) {
								//vv("not found")
								// start over
								k, v, err = cur.Get([]byte{0}, nil, SetRange)
								panicOn(err)
							} else {
								panicOn(err)
							}
						}
						_, _ = k, v
						if i%1e4 == 0 {
							//vv("reader %v at i=%v  sees key:'%v' -> val:'%v'", readslot, i, string(k), string(v))
						}
					}
					cur.Close()
					return nil
				})
				panicOn(err)
				//vv("SphynxReader returned err='%v'", err)

				gid := curGID()
				barrier.WaitAtGate(gid)
				//vv("reader gid %v has passed the barrierWaitAtGate", gid)

				pause := rand.Intn(100)
				time.Sleep(time.Millisecond * time.Duration(pause))

			} // endless for
		} // defn reader

		nread := 11
		// start a bunch of readers, staggered
		for k := 0; k < nread; k++ {
			pause := rand.Intn(121)
			time.Sleep(time.Millisecond * time.Duration(pause))
			go reader()
		}

		writer := func() {
			// add one and delete one
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			for {
				//vv("writer begin BlockUntil(nread=%v)", nread)
				barrier.BlockUntil(nread)
				//vv("writer back from BlockUntil(nread=%v)", nread)

				txn, err := env.NewWriteTxn()
				panicOn(err)
				//vv("writer has made a new txn")

				for i := 0; i < 3e5; i++ {

					kx := rand.Intn(100)
					k := []byte(fmt.Sprintf("writers_k%v", kx))
					v := []byte(fmt.Sprintf("writers_v%v", kx))

					// 90/10 put or delete
					rnd := rand.Intn(10)
					switch rnd {
					default:
						err := txn.Put(dbi, k, v, 0)
						panicOn(err)
					case 1: // 10% of the time, delete
						err := txn.Del(dbi, k, nil)
						if IsNotFound(err) {
							// ok, ignore. will be frequent.
						} else {
							panicOn(err)
							if i%1e4 == 0 {
								//vv("writer deleted k '%v' ok", string(k))
							}
						}
					}

					if i%1e5 == 0 {
						//vv("writer at i=%v  sees key:'%v' -> val:'%v'", i, string(k), string(v))
					}
				}
				panicOn(txn.Commit())

				// MDB_CORRUPTED: Located page was wrong type
				// means that we boneheadedly closed/deleted the files while the write
				// was in progress, probably due to an errant defer.Close().

				barrier.UnblockReaders()

				pause := rand.Intn(100)
				time.Sleep(time.Millisecond * time.Duration(pause))

			} // endless for
		}
		go writer()

		return env, nil
	}
	_ = openDB

	fdA, err := ioutil.TempFile("", "dbA-concur-lmdb")
	panicOn(err)
	pathA := fdA.Name()
	fdA.Close()
	vv("temp file A:  %v", pathA)

	fdB, err := ioutil.TempFile("", "dbB-concur-lmdb")
	panicOn(err)
	pathB := fdB.Name()
	fdB.Close()
	vv("temp file B:  %v", pathB)

	envA, err := openDB(pathA)
	panicOn(err)
	_ = envA

	envB, err := openDB(pathB)
	panicOn(err)
	_ = envB

	select {}
}
