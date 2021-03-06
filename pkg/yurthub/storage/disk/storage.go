package disk

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alibaba/openyurt/pkg/yurthub/storage"
	"k8s.io/klog"
)

const (
	cacheBaseDir = "/etc/kubernetes/cache/"
	tmpPrefix    = "tmp_"
)

type diskStorage struct {
	baseDir          string
	keyPendingStatus map[string]struct{}
	sync.RWMutex
}

// NewDiskStorage creates a storage.Store for caching data into local disk
func NewDiskStorage(dir string) (storage.Store, error) {
	if dir == "" {
		dir = cacheBaseDir
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}

	ds := &diskStorage{
		keyPendingStatus: make(map[string]struct{}, 0),
		baseDir:          dir,
	}

	err := ds.Recover("")
	if err != nil {
		klog.Errorf("could not recover local storage, %v, and skip the error", err)
	}
	return ds, nil
}

// Create new a file with key and contents
func (ds *diskStorage) Create(key string, contents []byte) error {
	if key == "" || len(contents) == 0 {
		return nil
	}

	if !ds.lockKey(key) {
		return storage.ErrStorageAccessConflict
	}
	defer ds.unLockKey(key)

	absKey := filepath.Join(ds.baseDir, key)
	if info, err := os.Stat(absKey); err != nil {
		if os.IsNotExist(err) {
			dir, _ := filepath.Split(absKey)
			if _, err := os.Stat(dir); err != nil {
				if os.IsNotExist(err) {
					if err = os.MkdirAll(dir, 0755); err != nil {
						return err
					}
				} else {
					return err
				}
			} else {
				// dir for key is already exist
			}
		} else {
			return err
		}
	} else if info.Mode().IsRegular() {
		file, err := os.OpenFile(absKey, os.O_RDWR, 0666)
		if err != nil {
			return err
		}
		defer file.Close()

		if err := file.Truncate(0); err != nil {
			return err
		}

		if _, err := file.Seek(0, 0); err != nil {
			return err
		}
	} else {
		klog.Errorf("%s is exist, but not recognized, %v", key, info.Mode())
		return nil
	}

	if err := ioutil.WriteFile(absKey, contents, 0600); err != nil {
		return err
	}

	return nil
}

// Delete delete file that specified by key
func (ds *diskStorage) Delete(key string) error {
	if key == "" {
		return nil
	}

	errs := make([]error, 0)
	if err := ds.delete(key); err != nil {
		errs = append(errs, err)
	}

	tmpKey := getTmpKey(key)
	if err := ds.delete(tmpKey); err != nil {
		errs = append(errs, err)
	}

	if len(errs) != 0 {
		return fmt.Errorf("%#+v", errs)
	}
	return nil
}

func (ds *diskStorage) delete(key string) error {
	if key == "" {
		return nil
	}

	if !ds.lockKey(key) {
		return storage.ErrStorageAccessConflict
	}
	defer ds.unLockKey(key)

	absKey := filepath.Join(ds.baseDir, key)
	info, err := os.Stat(absKey)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if info.Mode().IsRegular() {
		return os.Remove(absKey)
	}

	return nil
}

// Get get contents from the file that specified by key
func (ds *diskStorage) Get(key string) ([]byte, error) {
	return ds.get(filepath.Join(ds.baseDir, key))
}

func (ds *diskStorage) get(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}

	key := strings.TrimPrefix(path, ds.baseDir)
	if !ds.lockKey(key) {
		return nil, storage.ErrStorageAccessConflict
	}
	defer ds.unLockKey(key)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte{}, nil
		}
		return nil, fmt.Errorf("failed to get bytes for %s, %v", key, err)
	} else if info.Mode().IsRegular() {
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		return b, nil
	}

	return nil, fmt.Errorf("%s is exist, but not recognized, %v", key, info.Mode())
}

// ListKeys list all of keys for files
func (ds *diskStorage) ListKeys(key string) ([]string, error) {
	keys := make([]string, 0)
	absPath := filepath.Join(ds.baseDir, key)
	if info, err := os.Stat(absPath); err != nil {
		if os.IsNotExist(err) {
			return keys, nil
		}
		return keys, err
	} else if info.IsDir() {
		err := filepath.Walk(absPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if info.Mode().IsRegular() {
				_, file := filepath.Split(path)
				if !strings.HasPrefix(file, tmpPrefix) {
					keys = append(keys, strings.TrimPrefix(path, ds.baseDir))
				}
			}

			return nil
		})

		return keys, err
	} else if info.Mode().IsRegular() {
		keys = append(keys, key)
		return keys, nil
	}

	return keys, fmt.Errorf("failed to list keys because %s not recognized", key)
}

// List get all of contents for local files
func (ds *diskStorage) List(key string) ([][]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("key for list is empty")
	}

	bb := make([][]byte, 0)
	absKey := filepath.Join(ds.baseDir, key)
	info, err := os.Stat(absKey)
	if err != nil {
		if os.IsNotExist(err) {
			return bb, nil
		}
		klog.Errorf("filed to list bytes for (%s), %v", key, err)
		return nil, err
	} else if info.Mode().IsRegular() {
		b, err := ds.get(absKey)
		if err != nil {
			return nil, err
		}

		bb = append(bb, b)
		return bb, nil
	} else if info.Mode().IsDir() {
		err := filepath.Walk(absKey, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if info.Mode().IsRegular() {
				b, err := ds.get(path)
				if err != nil {
					klog.Warningf("failed to get bytes for %s when listing bytes, %v", path, err)
					return nil
				}

				bb = append(bb, b)
			}

			return nil
		})

		if err != nil {
			return nil, err
		}

		return bb, nil
	}

	return nil, fmt.Errorf("%s is exist, but not recognized, %v", key, info.Mode())
}

// Update update local file that specified by key with contents
func (ds *diskStorage) Update(key string, contents []byte) error {
	if key == "" || len(contents) == 0 {
		return nil
	}

	if !ds.lockKey(key) {
		return storage.ErrStorageAccessConflict
	}
	defer ds.unLockKey(key)

	dir, file := filepath.Split(key)
	tmpKey := filepath.Join(dir, fmt.Sprintf("%s%s", tmpPrefix, file))

	err := ds.Create(tmpKey, contents)
	if err != nil {
		return err
	}

	tmpPath := filepath.Join(ds.baseDir, tmpKey)
	absKey := filepath.Join(ds.baseDir, key)
	info, err := os.Stat(absKey)
	if err != nil {
		if !os.IsNotExist(err) {
			os.Remove(tmpPath)
			return err
		}
	} else if info.Mode().IsRegular() {
		if err := os.Remove(absKey); err != nil {
			os.Remove(tmpPath)
			return err
		}
	}

	return os.Rename(tmpPath, absKey)
}

// Recover recover storage error
func (ds *diskStorage) Recover(key string) error {
	dir := filepath.Join(ds.baseDir, key)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			_, file := filepath.Split(path)
			if strings.HasPrefix(file, tmpPrefix) {
				tmpKey := strings.TrimPrefix(path, ds.baseDir)
				key := getKey(tmpKey)
				keyPath := filepath.Join(ds.baseDir, key)
				if !ds.lockKey(key) {
					return nil
				}
				defer ds.unLockKey(key)

				iErr := os.Rename(path, keyPath)
				if iErr != nil {
					klog.V(2).Infof("failed to recover bytes %s, %v", tmpKey, err)
					return nil
				}
				klog.V(2).Infof("bytes %s recovered successfully", key)
			}
		}

		return nil
	})

	return err
}

func (ds *diskStorage) lockKey(key string) bool {
	if ds.isKeyPending(key) {
		return false
	}

	if !ds.setKeyPending(key) {
		return false
	}

	return true
}

func (ds *diskStorage) unLockKey(key string) {
	ds.Lock()
	defer ds.Unlock()
	delete(ds.keyPendingStatus, key)
}

func (ds *diskStorage) isKeyPending(key string) bool {
	ds.RLock()
	defer ds.RUnlock()
	if _, ok := ds.keyPendingStatus[key]; ok {
		return true
	}
	return false
}

func (ds *diskStorage) setKeyPending(key string) bool {
	ds.Lock()
	defer ds.Unlock()
	if _, ok := ds.keyPendingStatus[key]; ok {
		return false
	}

	ds.keyPendingStatus[key] = struct{}{}
	return true
}

func getTmpKey(key string) string {
	dir, file := filepath.Split(key)
	return filepath.Join(dir, fmt.Sprintf("%s%s", tmpPrefix, file))
}

func getKey(tmpKey string) string {
	dir, file := filepath.Split(tmpKey)
	return filepath.Join(dir, strings.TrimPrefix(file, tmpPrefix))
}
