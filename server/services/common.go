package services

import (
	"crypto/sha1"
	"fmt"
	"github.com/pkg/errors"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	DataDirFlag = "data-dir"
)

func DistributeByHash(dirs []string, hash string) (string, error) {
	sort.Strings(dirs)
	hex := fmt.Sprintf("%x", sha1.Sum([]byte(hash)))[0:5]
	num64, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse hex from hex=%v infohash=%v", hex, hash)
	}
	num := int(num64 * 1000)
	total := 1048575 * 1000
	interval := total / len(dirs)
	for i := 0; i < len(dirs); i++ {
		if num < (i+1)*interval {
			return dirs[i], nil
		}
	}
	return "", errors.Wrapf(err, "failed to distribute infohash=%v", hash)
}

func GetDir(location string, hash string) (string, error) {
	if strings.HasSuffix(location, "*") {
		prefix := strings.TrimSuffix(location, "*")
		dir, lp := path.Split(prefix)
		if dir == "" {
			dir = "."
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		dirs := []string{}
		for _, f := range files {
			if f.IsDir() && strings.HasPrefix(f.Name(), lp) {
				dirs = append(dirs, f.Name())
			}
		}
		if len(dirs) == 0 {
			return prefix + "/" + hash, nil
		} else if len(dirs) == 1 {
			return dir + "/" + dirs[0] + "/" + hash, nil
		} else {
			d, err := DistributeByHash(dirs, hash)
			if err != nil {
				return "", err
			}
			return dir + "/" + d + "/" + hash, nil
		}
	} else {
		return location + "/" + hash, nil
	}
}
