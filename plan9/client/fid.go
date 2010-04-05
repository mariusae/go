package client

import (
	"io"
	"io/ioutil"
	"os"
	"strings"
	"sync"

	"goplan9.googlecode.com/hg/plan9"
)

func getuser() string { return os.Getenv("USER") }

type Fid struct {
	c *Conn
	qid plan9.Qid
	fid uint32
	mode uint8
	offset int64
	f sync.Mutex
}

func (fid *Fid) Close() os.Error {
	if fid == nil {
		return nil
	}
	tx := &plan9.Fcall{Type: plan9.Tclunk, Fid: fid.fid}
	_, err := fid.c.rpc(tx)
	fid.c.putfid(fid)
	return err
}

func (fid *Fid) Create(name string, mode uint8, perm plan9.Perm) os.Error {
	tx := &plan9.Fcall{Type: plan9.Tcreate, Fid: fid.fid, Name: name, Mode: mode, Perm: perm}
	rx, err := fid.c.rpc(tx)
	if err != nil {
		return err
	}
	fid.mode = mode
	fid.qid = rx.Qid
	return nil
}

func (fid *Fid) Dirread() ([]*plan9.Dir, os.Error) {
	buf := make([]byte, plan9.STATMAX)
	n, err := fid.Read(buf)
	if err != nil {
		return nil, err
	}
	return dirUnpack(buf[0:n])
}

func (fid *Fid) Dirreadall() ([]*plan9.Dir, os.Error) {
	buf, err := ioutil.ReadAll(fid)
	if len(buf) == 0 {
		return nil, err
	}
	return dirUnpack(buf)
}

func dirUnpack(b []byte) ([]*plan9.Dir, os.Error) {
	var err os.Error
	dirs := make([]*plan9.Dir, 0, 10)
	for len(b) > 0 {
		if len(b) < 2 {
			err = io.ErrUnexpectedEOF
			break
		}
		n := int(b[0]) | int(b[1])<<8
		if len(b) < n+2 {
			err = io.ErrUnexpectedEOF
			break
		}
		d, err := plan9.UnmarshalDir(b[0:n+2])
		if err != nil {
			break
		}
		b = b[n+2:]
		if len(dirs) >= cap(dirs) {
			ndirs := make([]*plan9.Dir, len(dirs), 2*cap(dirs))
			copy(ndirs, dirs)
			dirs = ndirs
		}
		n = len(dirs)
		dirs = dirs[0:n+1]
		dirs[n] = d
	}
	return dirs, err
}

func (fid *Fid) Open(mode uint8) os.Error {
	tx := &plan9.Fcall{Type: plan9.Topen, Fid: fid.fid, Mode: mode}
	_, err := fid.c.rpc(tx)
	if err != nil {
		return err
	}
	fid.mode = mode
	return nil
}

func (fid *Fid) Qid() plan9.Qid {
	return fid.qid
}

func (fid *Fid) Read(b []byte) (n int, err os.Error) {
	return fid.ReadAt(b, -1)
}

func (fid *Fid) ReadAt(b []byte, offset int64) (n int, err os.Error) {
	msize := fid.c.msize - plan9.IOHDRSZ
	n = len(b)
	if uint32(n) > msize {
		n = int(msize)
	}
	o := offset
	if o == -1 {
		fid.f.Lock()
		o = fid.offset
		fid.f.Unlock()
	}
	tx := &plan9.Fcall{Type: plan9.Tread, Fid: fid.fid, Offset: uint64(o), Count: uint32(n)}
	rx, err := fid.c.rpc(tx)
	if err != nil {
		return 0, err
	}
	if len(rx.Data) == 0 {
		return 0, os.EOF
	}
	copy(b, rx.Data)
	if offset == -1 {
		fid.f.Lock()
		fid.offset += int64(len(rx.Data))
		fid.f.Unlock()
	}
	return len(rx.Data), nil
}

func (fid *Fid) ReadFull(b []byte) (n int, err os.Error) {
	return io.ReadFull(fid, b)
}

func (fid *Fid) Remove() os.Error {
	tx := &plan9.Fcall{Type: plan9.Tremove, Fid: fid.fid}
	_, err := fid.c.rpc(tx)
	fid.c.putfid(fid)
	return err
}

func (fid *Fid) Seek(n int64, whence int) (int64, os.Error) {
	switch whence {
	case 0:
		fid.f.Lock()
		fid.offset = n
		fid.f.Unlock()
		
	case 1:
		fid.f.Lock()
		n += fid.offset
		if n < 0 {
			fid.f.Unlock()
			return 0, Error("negative offset")
		}
		fid.offset = n
		fid.f.Unlock()
	
	case 2:
		d, err := fid.Stat()
		if err != nil {
			return 0, err
		}
		n += int64(d.Length)
		if n < 0 {
			return 0, Error("negative offset")
		}
		fid.f.Lock()
		fid.offset = n
		fid.f.Unlock()
	
	default:
		return 0, Error("bad whence in seek")
	}
	
	return n, nil
}

func (fid *Fid) Stat() (*plan9.Dir, os.Error) {
	tx := &plan9.Fcall{Type: plan9.Tstat, Fid: fid.fid}
	rx, err := fid.c.rpc(tx)
	if err != nil {
		return nil, err
	}
	return plan9.UnmarshalDir(rx.Stat)
}

// TODO(rsc): Could use ...string instead?
func (fid *Fid) Walk(name string) (*Fid, os.Error) {
	wfid, err := fid.c.newfid()
	if err != nil {
		return nil, err
	}
	
	// Split, delete empty strings and dot.
	elem := strings.Split(name, "/", -1)
	j := 0
	for _, e := range elem {
		if e != "" && e != "." {
			elem[j] = e
			j++
		}
	}
	elem = elem[0:j]

	for nwalk := 0;; nwalk++ {
		n := len(elem)
		if n > plan9.MAXWELEM {
			n = plan9.MAXWELEM
		}
		tx := &plan9.Fcall{Type: plan9.Twalk, Newfid: wfid.fid, Wname: elem[0:n]}
		if nwalk == 0 {
			tx.Fid = fid.fid
		} else {
			tx.Fid = wfid.fid
		}
		rx, err := fid.c.rpc(tx)
		if err == nil && len(rx.Wqid) != n {
			err = Error("file '"+name+"' not found")
		}
		if err != nil {
			if nwalk > 0 {
				wfid.Close()
			} else {
				fid.c.putfid(wfid)
			}
			return nil, err
		}
		if n == 0 {
			wfid.qid = fid.qid
		} else {
			wfid.qid = rx.Wqid[n-1]
		}
		elem = elem[n:]
		if len(elem) == 0 {
			break
		}
	}
	return wfid, nil
}

func (fid *Fid) Write(b []byte) (n int, err os.Error) {
	return fid.WriteAt(b, -1)
}

func (fid *Fid) WriteAt(b []byte, offset int64) (n int, err os.Error) {
	msize := fid.c.msize - plan9.IOHDRSIZE
	tot := 0
	n = len(b)
	first := true
	for tot < n || first {
		want := n - tot
		if uint32(want) > msize {
			want = int(msize)
		}
		got, err := fid.writeAt(b[tot:tot+want], offset)
		tot += got
		if err != nil {
			return tot, err
		}
		if offset != -1 {
			offset += int64(got)
		}
		first = false
	}
	return tot, nil
}

func (fid *Fid) writeAt(b []byte, offset int64) (n int, err os.Error) {
	o := offset
	if o == -1 {
		fid.f.Lock()
		o = fid.offset
		fid.f.Unlock()
	}
	tx := &plan9.Fcall{Type: plan9.Twrite, Fid: fid.fid, Offset: uint64(o), Data: b}
	rx, err := fid.c.rpc(tx)
	if err != nil {
		return 0, err
	}
	if o == -1 && rx.Count > 0 {
		fid.f.Lock()
		fid.offset += int64(rx.Count)
		fid.f.Unlock()
	}
	return int(rx.Count), nil
}

func (fid *Fid) Wstat(d *plan9.Dir) os.Error {
	b, err := d.Bytes()
	if err != nil {
		return err
	}
	tx := &plan9.Fcall{Type: plan9.Twstat, Fid: fid.fid, Stat: b}
	_, err = fid.c.rpc(tx)
	return err
}