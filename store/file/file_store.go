// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by quickfixengine.org and appearing in the file
// LICENSE included in the packaging of this file.
//
// This file is provided AS IS with NO WARRANTY OF ANY KIND, INCLUDING
// THE WARRANTY OF DESIGN, MERCHANTABILITY AND FITNESS FOR A
// PARTICULAR PURPOSE.
//
// See http://www.quickfixengine.org/LICENSE for licensing information.
//
// Contact ask@quickfixengine.org if any conditions of this licensing
// are not clear to you.

package file

import (
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/quickfix/config"
)

type fileStoreFactory struct {
	settings *quickfix.Settings
}

type fileStore struct {
	sessionID          quickfix.SessionID
	cache              quickfix.MessageStore
	bodyFname          string
	headerFname        string
	sessionFname       string
	senderSeqNumsFname string
	targetSeqNumsFname string

	fileMu            sync.Mutex
	bodyFile          *os.File
	headerFile        *os.File
	sessionFile       *os.File
	senderSeqNumsFile *os.File
	targetSeqNumsFile *os.File
	fileSync          bool
}

// NewStoreFactory returns a file-based implementation of MessageStoreFactory.
func NewStoreFactory(settings *quickfix.Settings) quickfix.MessageStoreFactory {
	return fileStoreFactory{settings: settings}
}

// Create creates a new FileStore implementation of the MessageStore interface.
func (f fileStoreFactory) Create(sessionID quickfix.SessionID) (msgStore quickfix.MessageStore, err error) {
	globalSettings := f.settings.GlobalSettings()
	dynamicSessions, _ := globalSettings.BoolSetting(config.DynamicSessions)

	sessionSettings, ok := f.settings.SessionSettings()[sessionID]
	if !ok {
		if dynamicSessions {
			sessionSettings = globalSettings
		} else {
			return nil, fmt.Errorf("unknown session: %v", sessionID)
		}
	}

	dirname, err := sessionSettings.Setting(config.FileStorePath)
	if err != nil {
		return nil, err
	}
	var fsync bool
	if sessionSettings.HasSetting(config.FileStoreSync) {
		fsync, err = sessionSettings.BoolSetting(config.FileStoreSync)
		if err != nil {
			return nil, err
		}
	} else {
		fsync = true //existing behavior is to fsync writes
	}
	return newFileStore(sessionID, dirname, fsync)
}

func newFileStore(sessionID quickfix.SessionID, dirname string, fileSync bool) (*fileStore, error) {
	if err := os.MkdirAll(dirname, os.ModePerm); err != nil {
		return nil, err
	}

	sessionPrefix := createFilenamePrefix(sessionID)

	memStore, memErr := quickfix.NewMemoryStoreFactory().Create(sessionID)
	if memErr != nil {
		return nil, errors.Wrap(memErr, "cache creation")
	}

	store := &fileStore{
		sessionID:          sessionID,
		cache:              memStore,
		bodyFname:          path.Join(dirname, fmt.Sprintf("%s.%s", sessionPrefix, "body")),
		headerFname:        path.Join(dirname, fmt.Sprintf("%s.%s", sessionPrefix, "header")),
		sessionFname:       path.Join(dirname, fmt.Sprintf("%s.%s", sessionPrefix, "session")),
		senderSeqNumsFname: path.Join(dirname, fmt.Sprintf("%s.%s", sessionPrefix, "senderseqnums")),
		targetSeqNumsFname: path.Join(dirname, fmt.Sprintf("%s.%s", sessionPrefix, "targetseqnums")),
		fileSync:           fileSync,
	}

	if err := store.Refresh(); err != nil {
		return nil, err
	}

	return store, nil
}

// Reset deletes the store files and sets the seqnums back to 1.
func (store *fileStore) Reset() error {
	if err := store.cache.Reset(); err != nil {
		return errors.Wrap(err, "cache reset")
	}

	if err := store.Close(); err != nil {
		return errors.Wrap(err, "close")
	}
	if err := removeFile(store.bodyFname); err != nil {
		return err
	}
	if err := removeFile(store.headerFname); err != nil {
		return err
	}
	if err := removeFile(store.sessionFname); err != nil {
		return err
	}
	if err := removeFile(store.senderSeqNumsFname); err != nil {
		return err
	}
	if err := removeFile(store.targetSeqNumsFname); err != nil {
		return err
	}
	return store.Refresh()
}

// Refresh closes the store files and then reloads from them.
func (store *fileStore) Refresh() (err error) {
	if err = store.cache.Reset(); err != nil {
		err = errors.Wrap(err, "cache reset")
		return
	}

	if err = store.Close(); err != nil {
		return err
	}

	creationTimePopulated, err := store.populateCache()
	if err != nil {
		return err
	}

	if store.bodyFile, err = openOrCreateFile(store.bodyFname, 0660); err != nil {
		return err
	}
	if store.headerFile, err = openOrCreateFile(store.headerFname, 0660); err != nil {
		return err
	}
	if store.sessionFile, err = openOrCreateFile(store.sessionFname, 0660); err != nil {
		return err
	}
	if store.senderSeqNumsFile, err = openOrCreateFile(store.senderSeqNumsFname, 0660); err != nil {
		return err
	}
	if store.targetSeqNumsFile, err = openOrCreateFile(store.targetSeqNumsFname, 0660); err != nil {
		return err
	}

	if !creationTimePopulated {
		if err := store.setSession(); err != nil {
			return err
		}
	}

	if err := store.SetNextSenderMsgSeqNum(store.NextSenderMsgSeqNum()); err != nil {
		return errors.Wrap(err, "set next sender")
	}

	if err := store.SetNextTargetMsgSeqNum(store.NextTargetMsgSeqNum()); err != nil {
		return errors.Wrap(err, "set next target")
	}
	return nil
}

func (store *fileStore) populateCache() (creationTimePopulated bool, err error) {
	if timeBytes, err := os.ReadFile(store.sessionFname); err == nil {
		var ctime time.Time
		if err := ctime.UnmarshalText(timeBytes); err == nil {
			store.cache.SetCreationTime(ctime)
			creationTimePopulated = true
		}
	}

	if senderSeqNumBytes, err := os.ReadFile(store.senderSeqNumsFname); err == nil {
		if senderSeqNum, err := strconv.Atoi(strings.Trim(string(senderSeqNumBytes), "\r\n")); err == nil {
			if err = store.cache.SetNextSenderMsgSeqNum(senderSeqNum); err != nil {
				return creationTimePopulated, errors.Wrap(err, "cache set next sender")
			}
		}
	}

	if targetSeqNumBytes, err := os.ReadFile(store.targetSeqNumsFname); err == nil {
		if targetSeqNum, err := strconv.Atoi(strings.Trim(string(targetSeqNumBytes), "\r\n")); err == nil {
			if err = store.cache.SetNextTargetMsgSeqNum(targetSeqNum); err != nil {
				return creationTimePopulated, errors.Wrap(err, "cache set next target")
			}
		}
	}

	return creationTimePopulated, nil
}

func (store *fileStore) setSession() error {
	store.fileMu.Lock()
	defer store.fileMu.Unlock()

	if _, err := store.sessionFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind file: %s: %s", store.sessionFname, err.Error())
	}

	data, err := store.cache.CreationTime().MarshalText()
	if err != nil {
		return fmt.Errorf("unable to marshal session time to file: %s: %s", store.sessionFname, err.Error())
	}
	if _, err := store.sessionFile.Write(data); err != nil {
		return fmt.Errorf("unable to write to file: %s: %s", store.sessionFname, err.Error())
	}
	if store.fileSync {
		if err := store.sessionFile.Sync(); err != nil {
			return fmt.Errorf("unable to flush file: %s: %s", store.sessionFname, err.Error())
		}
	}
	return nil
}

func (store *fileStore) setSeqNum(f *os.File, seqNum int) error {
	store.fileMu.Lock()
	defer store.fileMu.Unlock()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind file: %s: %s", f.Name(), err.Error())
	}
	if _, err := fmt.Fprintf(f, "%019d", seqNum); err != nil {
		return fmt.Errorf("unable to write to file: %s: %s", f.Name(), err.Error())
	}
	if store.fileSync {
		if err := f.Sync(); err != nil {
			return fmt.Errorf("unable to flush file: %s: %s", f.Name(), err.Error())
		}
	}
	return nil
}

// NextSenderMsgSeqNum returns the next MsgSeqNum that will be sent.
func (store *fileStore) NextSenderMsgSeqNum() int {
	return store.cache.NextSenderMsgSeqNum()
}

// NextTargetMsgSeqNum returns the next MsgSeqNum that should be received.
func (store *fileStore) NextTargetMsgSeqNum() int {
	return store.cache.NextTargetMsgSeqNum()
}

// SetNextSenderMsgSeqNum sets the next MsgSeqNum that will be sent.
func (store *fileStore) SetNextSenderMsgSeqNum(next int) error {
	if err := store.setSeqNum(store.senderSeqNumsFile, next); err != nil {
		return errors.Wrap(err, "file")
	}
	return store.cache.SetNextSenderMsgSeqNum(next)
}

// SetNextTargetMsgSeqNum sets the next MsgSeqNum that should be received.
func (store *fileStore) SetNextTargetMsgSeqNum(next int) error {
	if err := store.setSeqNum(store.targetSeqNumsFile, next); err != nil {
		return errors.Wrap(err, "file")
	}
	return store.cache.SetNextTargetMsgSeqNum(next)
}

// IncrNextSenderMsgSeqNum increments the next MsgSeqNum that will be sent.
func (store *fileStore) IncrNextSenderMsgSeqNum() error {
	if err := store.SetNextSenderMsgSeqNum(store.cache.NextSenderMsgSeqNum() + 1); err != nil {
		return errors.Wrap(err, "file")
	}
	return nil
}

// IncrNextTargetMsgSeqNum increments the next MsgSeqNum that should be received.
func (store *fileStore) IncrNextTargetMsgSeqNum() error {
	if err := store.SetNextTargetMsgSeqNum(store.cache.NextTargetMsgSeqNum() + 1); err != nil {
		return errors.Wrap(err, "file")
	}
	return nil
}

// CreationTime returns the creation time of the store.
func (store *fileStore) CreationTime() time.Time {
	return store.cache.CreationTime()
}

// SetCreationTime is a no-op for FileStore.
func (store *fileStore) SetCreationTime(_ time.Time) {
}

func (store *fileStore) SaveMessage(seqNum int, msg []byte) error {
	store.fileMu.Lock()
	defer store.fileMu.Unlock()
	offset, err := store.bodyFile.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("unable to seek to end of file: %s: %s", store.bodyFname, err.Error())
	}
	if _, err := store.headerFile.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("unable to seek to end of file: %s: %s", store.headerFname, err.Error())
	}
	if _, err := fmt.Fprintf(store.headerFile, "%d,%d,%d\n", seqNum, offset, len(msg)); err != nil {
		return fmt.Errorf("unable to write to file: %s: %s", store.headerFname, err.Error())
	}

	if _, err := store.bodyFile.Write(msg); err != nil {
		return fmt.Errorf("unable to write to file: %s: %s", store.bodyFname, err.Error())
	}
	if store.fileSync {
		return store.syncBodyAndHeaderFilesLocked()
	}
	return nil
}

func (store *fileStore) SaveMessageAndIncrNextSenderMsgSeqNum(seqNum int, msg []byte) error {
	err := store.SaveMessage(seqNum, msg)
	if err != nil {
		return err
	}
	return store.IncrNextSenderMsgSeqNum()
}

func (store *fileStore) syncBodyAndHeaderFilesLocked() error {
	if err := store.bodyFile.Sync(); err != nil {
		return fmt.Errorf("unable to flush file: %s: %s", store.bodyFname, err.Error())
	} else if err = store.headerFile.Sync(); err != nil {
		return fmt.Errorf("unable to flush file: %s: %s", store.headerFname, err.Error())
	}
	return nil
}

func (store *fileStore) IterateMessages(beginSeqNum, endSeqNum int, cb func([]byte) error) error {
	// Sync files
	store.fileMu.Lock()
	err := store.syncBodyAndHeaderFilesLocked()
	store.fileMu.Unlock()
	if err != nil {
		return err
	}

	// Open a read only view to body and header file
	bodyFile, err := openOrCreateFile(store.bodyFname, 0440)
	if err != nil {
		return err
	}
	defer func() { _ = bodyFile.Close() }()
	headerFile, err := openOrCreateFile(store.headerFname, 0440)
	if err != nil {
		return err
	}
	defer func() { _ = headerFile.Close() }()
	if _, err = headerFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to seek to start of file: %s: %s", store.headerFname, err.Error())
	}

	// Iterate over the header file
	for {
		var seqNum, size int
		var offset int64
		if cnt, err := fmt.Fscanf(headerFile, "%d,%d,%d\n", &seqNum, &offset, &size); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("unable to read from file: %s: %s", store.headerFname, err.Error())
		} else if cnt < 3 || seqNum > endSeqNum {
			// If we have reached the end of possible iteration then break
			break
		} else if seqNum < beginSeqNum {
			// If we have not yet reached the starting sequence number then continue
			continue
		}
		// Otherwise process the file
		msg := make([]byte, size)
		if _, err := bodyFile.ReadAt(msg, offset); err != nil {
			return fmt.Errorf("unable to read from file: %s: %s", store.bodyFname, err.Error())
		} else if err = cb(msg); err != nil {
			return err
		}
	}
	return nil
}

func (store *fileStore) GetMessages(beginSeqNum, endSeqNum int) ([][]byte, error) {
	var msgs [][]byte
	err := store.IterateMessages(beginSeqNum, endSeqNum, func(msg []byte) error {
		msgs = append(msgs, msg)
		return nil
	})
	return msgs, err
}

// Close closes the store's files.
func (store *fileStore) Close() error {
	if err := closeSyncFile(store.bodyFile); err != nil {
		return err
	}
	if err := closeSyncFile(store.headerFile); err != nil {
		return err
	}
	if err := closeSyncFile(store.sessionFile); err != nil {
		return err
	}
	if err := closeSyncFile(store.senderSeqNumsFile); err != nil {
		return err
	}
	if err := closeSyncFile(store.targetSeqNumsFile); err != nil {
		return err
	}

	store.bodyFile = nil
	store.headerFile = nil
	store.sessionFile = nil
	store.senderSeqNumsFile = nil
	store.targetSeqNumsFile = nil

	return nil
}
