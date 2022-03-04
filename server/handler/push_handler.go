package handler

import (
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/no-src/gofs/action"
	"github.com/no-src/gofs/contract"
	"github.com/no-src/gofs/contract/push"
	"github.com/no-src/gofs/core"
	"github.com/no-src/gofs/fs"
	"github.com/no-src/gofs/server"
	"github.com/no-src/gofs/util"
	"github.com/no-src/log"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type pushHandler struct {
	logger                log.Logger
	storagePath           string
	enableLogicallyDelete bool
}

// NewPushHandler create an instance of the pushHandler
func NewPushHandler(logger log.Logger, source core.VFS, enableLogicallyDelete bool) GinHandler {
	return &pushHandler{
		logger:                logger,
		storagePath:           source.Path(),
		enableLogicallyDelete: enableLogicallyDelete,
	}
}

func (h *pushHandler) Handle(c *gin.Context) {
	defer func() {
		e := recover()
		if e != nil {
			c.JSON(http.StatusOK, server.NewServerErrorResult())
		}
	}()

	fileInfo := c.PostForm(push.FileInfo)
	offset, _ := strconv.ParseInt(c.PostForm(push.Offset), 10, 64)
	var pushData push.PushData
	err := util.Unmarshal([]byte(fileInfo), &pushData)
	if err != nil {
		msg := "unmarshal file info error"
		c.JSON(http.StatusOK, server.NewErrorApiResult(-501, msg))
		h.logger.Error(err, "%s => %s", msg, fileInfo)
		return
	}

	h.logger.Debug("receive action %s => %s", pushData.Action.String(), fileInfo)

	if pushData.Action.Valid() == action.UnknownAction {
		c.JSON(http.StatusOK, server.NewErrorApiResult(-502, fmt.Sprintf("unknown action => %d", pushData.Action.Int())))
		return
	}
	fi := pushData.FileInfo
	switch pushData.Action {
	case action.CreateAction:
		err = h.create(fi)
		break
	case action.RemoveAction:
		err = h.remove(fi)
		break
	case action.RenameAction:
		err = h.rename(fi)
		break
	case action.ChmodAction:
		err = h.chmod(fi)
		break
	case action.WriteAction:
		r, _ := h.write(fi, offset, c)
		c.JSON(http.StatusOK, r)
		return
	default:
		err = fmt.Errorf("unsupported action => [%d:%s]", pushData.Action.Int(), pushData.Action.String())
	}
	if err != nil {
		h.logger.Error(err, "process action error %s => %s", pushData.Action.String(), fi.Path)
		c.JSON(http.StatusOK, server.NewErrorApiResult(-503, fmt.Sprintf("process action error => %s", err.Error())))
	} else {
		c.JSON(http.StatusOK, server.NewApiResult(contract.Success, contract.SuccessDesc, nil))
	}
}

func (h *pushHandler) buildAbsPath(path string) string {
	return filepath.Join(h.storagePath, path)
}

func (h *pushHandler) create(fi contract.FileInfo) error {
	path := h.buildAbsPath(fi.Path)
	exist, err := fs.FileExist(path)
	if err != nil {
		return err
	}
	if exist {
		return nil
	}
	if fi.IsDir.Bool() {
		err = os.MkdirAll(path, os.ModePerm)
		if err != nil {
			return err
		}
	} else {
		dir := filepath.Dir(path)
		err = os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			return err
		}
		f, err := fs.CreateFile(path)
		defer func() {
			if err = f.Close(); err != nil {
				h.logger.Error(err, "close file error")
			}
		}()
		if err != nil {
			return err
		}
	}

	err = h.chtimes(path, fi)
	if err != nil {
		return err
	}
	h.logger.Info("create the dest file success [%s]", path)
	return nil
}

func (h *pushHandler) remove(fi contract.FileInfo) (err error) {
	path := h.buildAbsPath(fi.Path)
	if h.enableLogicallyDelete {
		err = fs.LogicallyDelete(path)
	} else {
		err = os.RemoveAll(path)
	}
	if err == nil {
		h.logger.Info("remove file success [%s]", path)
	}
	return err
}

func (h *pushHandler) rename(fi contract.FileInfo) (err error) {
	path := h.buildAbsPath(fi.Path)
	err = os.RemoveAll(path)
	if err == nil {
		h.logger.Info("remove file success [%s]", path)
	}
	return err
}

func (h *pushHandler) chmod(fi contract.FileInfo) (err error) {
	path := h.buildAbsPath(fi.Path)
	h.logger.Debug("chmod is unimplemented [%s]", path)
	return nil
}

func (h *pushHandler) write(fi contract.FileInfo, offset int64, c *gin.Context) (server.ApiResult, error) {
	if fi.IsDir.Bool() {
		err := errors.New("can't write a directory")
		h.logger.Error(err, "write upload file error")
		return server.NewErrorApiResult(-504, err.Error()), err
	}
	path := h.buildAbsPath(fi.Path)
	fh, err := c.FormFile(push.UpFile)
	if err != nil {
		msg := "get upload file error"
		h.logger.Error(err, msg)
		return server.NewErrorApiResult(-505, msg), err
	}

	abort, err := h.Save(fh, path, offset, fi.Hash, fi.Size)
	if err != nil {
		h.logger.Error(err, fmt.Sprintf("save upload file error => [%s]", path))
		return server.NewErrorApiResult(-506, fmt.Sprintf("save upload file error => [%s]", fi.Path)), err
	} else if abort {
		h.logger.Debug("upload a file that is not modified, ignore and abort the next request => %s", fi.Path)
		return server.NewApiResult(contract.Abort, contract.AbortDesc, nil), nil
	}

	// change file times
	if err = h.chtimes(path, fi); err != nil {
		log.Error(err, "change file times error after write file => [%s]", path)
		return server.NewErrorApiResult(-507, fmt.Sprintf("change file times error => [%s]", fi.Path)), err
	}

	return server.NewApiResult(contract.Success, contract.SuccessDesc, nil), nil
}

func (h *pushHandler) chtimes(absPath string, fi contract.FileInfo) error {
	return os.Chtimes(absPath, time.Unix(fi.ATime, 0), time.Unix(fi.MTime, 0))
}

func (h *pushHandler) Save(file *multipart.FileHeader, dst string, offset int64, hash string, size int64) (abort bool, err error) {
	// the offset less than zero means to compare file size and hash value only
	if offset < 0 {
		if h.compare(dst, hash, size) {
			return true, nil
		}
		return abort, nil
	}
	src, err := file.Open()
	if err != nil {
		return abort, err
	}
	defer src.Close()

	var out *os.File
	if offset > 0 {
		out, err = os.OpenFile(dst, os.O_APPEND|os.O_CREATE, 0666)
	} else {
		out, err = os.Create(dst)
	}

	if err != nil {
		return abort, err
	}
	defer out.Close()

	if offset > 0 {
		_, err = out.Seek(offset, 0)
		if err != nil {
			return abort, err
		}
	}

	_, err = io.Copy(out, src)
	return abort, err
}

// compare compare file size and hash value
func (h *pushHandler) compare(dstPath string, sourceHash string, sourceSize int64) (equal bool) {
	if sourceSize > 0 && len(sourceHash) > 0 {
		destStat, err := os.Stat(dstPath)
		if err == nil && destStat.Size() == sourceSize {
			destHash, err := util.MD5FromFileName(dstPath)
			if err == nil && destHash == sourceHash {
				return true
			}
		}
	}
	return false
}
