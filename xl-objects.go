/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/minio/minio/pkg/mimedb"
)

const (
	multipartSuffix   = ".minio.multipart"
	multipartMetaFile = "00000" + multipartSuffix
	formatConfigFile  = "format.json"
)

// xlObjects - Implements fs object layer.
type xlObjects struct {
	storage            StorageAPI
	listObjectMap      map[listParams][]*treeWalker
	listObjectMapMutex *sync.Mutex
}

// isValidFormat - validates input arguments with backend 'format.json'
func isValidFormat(storage StorageAPI, exportPaths ...string) bool {
	// Load saved XL format.json and validate.
	xl, err := loadFormatXL(storage)
	if err != nil {
		log.Errorf("loadFormatXL failed with %s", err)
		return false
	}
	if xl.Version != "1" {
		log.Errorf("Unsupported XL backend format found [%s]", xl.Version)
		return false
	}
	if len(exportPaths) != len(xl.Disks) {
		log.Errorf("Number of disks %d passed at the command-line did not match the backend format %d", len(exportPaths), len(xl.Disks))
		return false
	}
	for index, disk := range xl.Disks {
		if exportPaths[index] != disk {
			log.Errorf("Invalid order of disks detected %s. Required order is %s.", exportPaths, xl.Disks)
			return false
		}
	}
	return true
}

// newXLObjects - initialize new xl object layer.
func newXLObjects(exportPaths ...string) (ObjectLayer, error) {
	storage, err := newXL(exportPaths...)
	if err != nil {
		log.Errorf("newXL failed with %s", err)
		return nil, err
	}

	// Initialize object layer - like creating minioMetaBucket,
	// cleaning up tmp files etc.
	initObjectLayer(storage)

	err = checkFormat(storage)
	if err != nil {
		if err == errFileNotFound {
			// Save new XL format.
			errSave := saveFormatXL(storage, &xlFormat{
				Version: "1",
				Disks:   exportPaths,
			})
			if errSave != nil {
				log.Errorf("saveFormatXL failed with %s", errSave)
				return nil, errSave
			}
		} else {
			log.Errorf("Unable to check backend format %s", err)
			if err == errReadQuorum {
				errMsg := fmt.Sprintf("Not all disks %s are available, did not meet read quroum.", exportPaths)
				err = errors.New(errMsg)
			} else if err == errDiskNotFound {
				errMsg := fmt.Sprintf("Disks %s not found.", exportPaths)
				err = errors.New(errMsg)
			} else if err == errVolumeAccessDenied {
				errMsg := fmt.Sprintf("Disks %s access permission denied.", exportPaths)
				err = errors.New(errMsg)
			}
			return nil, err
		}
	}

	// Validate if format exists and input arguments are validated
	// with backend format.
	if !isValidFormat(storage, exportPaths...) {
		return nil, fmt.Errorf("Command-line arguments %s is not valid.", exportPaths)
	}

	// Return successfully initialized object layer.
	return xlObjects{
		storage:            storage,
		listObjectMap:      make(map[listParams][]*treeWalker),
		listObjectMapMutex: &sync.Mutex{},
	}, nil
}

/// Bucket operations

// MakeBucket - make a bucket.
func (xl xlObjects) MakeBucket(bucket string) error {
	return makeBucket(xl.storage, bucket)
}

// GetBucketInfo - get bucket info.
func (xl xlObjects) GetBucketInfo(bucket string) (BucketInfo, error) {
	return getBucketInfo(xl.storage, bucket)
}

// ListBuckets - list buckets.
func (xl xlObjects) ListBuckets() ([]BucketInfo, error) {
	return listBuckets(xl.storage)
}

// DeleteBucket - delete a bucket.
func (xl xlObjects) DeleteBucket(bucket string) error {
	return deleteBucket(xl.storage, bucket)
}

/// Object Operations

// GetObject - get an object.
func (xl xlObjects) GetObject(bucket, object string, startOffset int64) (io.ReadCloser, error) {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return nil, BucketNameInvalid{Bucket: bucket}
	}
	if !isBucketExist(xl.storage, bucket) {
		return nil, BucketNotFound{Bucket: bucket}
	}
	// Verify if object is valid.
	if !IsValidObjectName(object) {
		return nil, ObjectNameInvalid{Bucket: bucket, Object: object}
	}
	if ok, err := isMultipartObject(xl.storage, bucket, object); err != nil {
		return nil, toObjectErr(err, bucket, object)
	} else if !ok {
		if _, err = xl.storage.StatFile(bucket, object); err == nil {
			var reader io.ReadCloser
			reader, err = xl.storage.ReadFile(bucket, object, startOffset)
			if err != nil {
				return nil, toObjectErr(err, bucket, object)
			}
			return reader, nil
		}
		return nil, toObjectErr(err, bucket, object)
	}
	fileReader, fileWriter := io.Pipe()
	info, err := getMultipartObjectInfo(xl.storage, bucket, object)
	if err != nil {
		return nil, toObjectErr(err, bucket, object)
	}
	partIndex, offset, err := info.GetPartNumberOffset(startOffset)
	if err != nil {
		return nil, toObjectErr(err, bucket, object)
	}
	go func() {
		for ; partIndex < len(info.Parts); partIndex++ {
			part := info.Parts[partIndex]
			r, err := xl.storage.ReadFile(bucket, pathJoin(object, partNumToPartFileName(part.PartNumber)), offset)
			if err != nil {
				fileWriter.CloseWithError(err)
				return
			}
			// Reset offset to 0 as it would be non-0 only for the first loop if startOffset is non-0.
			offset = 0
			if _, err = io.Copy(fileWriter, r); err != nil {
				switch reader := r.(type) {
				case *io.PipeReader:
					reader.CloseWithError(err)
				case io.ReadCloser:
					reader.Close()
				}
				fileWriter.CloseWithError(err)
				return
			}
			// Close the readerCloser that reads multiparts of an object from the xl storage layer.
			// Not closing leaks underlying file descriptors.
			r.Close()
		}
		fileWriter.Close()
	}()
	return fileReader, nil
}

// Return the partsInfo of a special multipart object.
func getMultipartObjectInfo(storage StorageAPI, bucket, object string) (info MultipartObjectInfo, err error) {
	offset := int64(0)
	r, err := storage.ReadFile(bucket, pathJoin(object, multipartMetaFile), offset)
	if err != nil {
		return MultipartObjectInfo{}, err
	}
	decoder := json.NewDecoder(r)
	err = decoder.Decode(&info)
	if err != nil {
		return MultipartObjectInfo{}, err
	}
	return info, nil
}

// Return ObjectInfo.
func (xl xlObjects) getObjectInfo(bucket, object string) (ObjectInfo, error) {
	// First see if the object was a simple-PUT upload.
	fi, err := xl.storage.StatFile(bucket, object)
	if err != nil {
		if err != errFileNotFound {
			return ObjectInfo{}, err
		}
		var info MultipartObjectInfo
		// Check if the object was multipart upload.
		info, err = getMultipartObjectInfo(xl.storage, bucket, object)
		if err != nil {
			return ObjectInfo{}, err
		}
		fi.Size = info.Size
		fi.ModTime = info.ModTime
		fi.MD5Sum = info.MD5Sum
	}
	contentType := "application/octet-stream"
	if objectExt := filepath.Ext(object); objectExt != "" {
		content, ok := mimedb.DB[strings.ToLower(strings.TrimPrefix(objectExt, "."))]
		if ok {
			contentType = content.ContentType
		}
	}
	return ObjectInfo{
		Bucket:      bucket,
		Name:        object,
		ModTime:     fi.ModTime,
		Size:        fi.Size,
		IsDir:       fi.Mode.IsDir(),
		ContentType: contentType,
		MD5Sum:      fi.MD5Sum,
	}, nil
}

// GetObjectInfo - get object info.
func (xl xlObjects) GetObjectInfo(bucket, object string) (ObjectInfo, error) {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return ObjectInfo{}, BucketNameInvalid{Bucket: bucket}
	}
	// Check whether the bucket exists.
	if !isBucketExist(xl.storage, bucket) {
		return ObjectInfo{}, BucketNotFound{Bucket: bucket}
	}
	// Verify if object is valid.
	if !IsValidObjectName(object) {
		return ObjectInfo{}, ObjectNameInvalid{Bucket: bucket, Object: object}
	}
	info, err := xl.getObjectInfo(bucket, object)
	if err != nil {
		return ObjectInfo{}, toObjectErr(err, bucket, object)
	}
	return info, nil
}

// PutObject - create an object.
func (xl xlObjects) PutObject(bucket string, object string, size int64, data io.Reader, metadata map[string]string) (string, error) {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return "", BucketNameInvalid{Bucket: bucket}
	}
	// Check whether the bucket exists.
	if !isBucketExist(xl.storage, bucket) {
		return "", BucketNotFound{Bucket: bucket}
	}
	if !IsValidObjectName(object) {
		return "", ObjectNameInvalid{
			Bucket: bucket,
			Object: object,
		}
	}

	tempObj := path.Join(tmpMetaPrefix, bucket, object)
	fileWriter, err := xl.storage.CreateFile(minioMetaBucket, tempObj)
	if err != nil {
		return "", toObjectErr(err, bucket, object)
	}

	// Initialize md5 writer.
	md5Writer := md5.New()

	// Instantiate a new multi writer.
	multiWriter := io.MultiWriter(md5Writer, fileWriter)

	// Instantiate checksum hashers and create a multiwriter.
	if size > 0 {
		if _, err = io.CopyN(multiWriter, data, size); err != nil {
			if clErr := safeCloseAndRemove(fileWriter); clErr != nil {
				return "", toObjectErr(clErr, bucket, object)
			}
			return "", toObjectErr(err, bucket, object)
		}
	} else {
		if _, err = io.Copy(multiWriter, data); err != nil {
			if clErr := safeCloseAndRemove(fileWriter); clErr != nil {
				return "", toObjectErr(clErr, bucket, object)
			}
			return "", toObjectErr(err, bucket, object)
		}
	}

	newMD5Hex := hex.EncodeToString(md5Writer.Sum(nil))
	// md5Hex representation.
	var md5Hex string
	if len(metadata) != 0 {
		md5Hex = metadata["md5Sum"]
	}
	if md5Hex != "" {
		if newMD5Hex != md5Hex {
			if err = safeCloseAndRemove(fileWriter); err != nil {
				return "", toObjectErr(err, bucket, object)
			}
			return "", BadDigest{md5Hex, newMD5Hex}
		}
	}
	err = fileWriter.Close()
	if err != nil {
		if clErr := safeCloseAndRemove(fileWriter); clErr != nil {
			return "", toObjectErr(clErr, bucket, object)
		}
		return "", toObjectErr(err, bucket, object)
	}

	// check if an object is present as one of the parent dir.
	if err = xl.parentDirIsObject(bucket, path.Dir(object)); err != nil {
		return "", toObjectErr(err, bucket, object)
	}

	// Delete if an object already exists.
	// FIXME: rename it to tmp file and delete only after
	// the newly uploaded file is renamed from tmp location to
	// the original location.
	err = xl.deleteObject(bucket, object)
	if err != nil && err != errFileNotFound {
		return "", toObjectErr(err, bucket, object)
	}
	err = xl.storage.RenameFile(minioMetaBucket, tempObj, bucket, object)
	if err != nil {
		if derr := xl.storage.DeleteFile(minioMetaBucket, tempObj); derr != nil {
			return "", toObjectErr(derr, bucket, object)
		}
		return "", toObjectErr(err, bucket, object)
	}

	// Return md5sum, successfully wrote object.
	return newMD5Hex, nil
}

// isMultipartObject - verifies if an object is special multipart file.
func isMultipartObject(storage StorageAPI, bucket, object string) (bool, error) {
	_, err := storage.StatFile(bucket, pathJoin(object, multipartMetaFile))
	if err != nil {
		if err == errFileNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Deletes and object.
func (xl xlObjects) deleteObject(bucket, object string) error {
	// Verify if the object is a multipart object.
	if ok, err := isMultipartObject(xl.storage, bucket, object); err != nil {
		return err
	} else if !ok {
		if err = xl.storage.DeleteFile(bucket, object); err != nil {
			return err
		}
		return nil
	}
	// Get parts info.
	info, err := getMultipartObjectInfo(xl.storage, bucket, object)
	if err != nil {
		return err
	}
	// Range through all files and delete it.
	var wg = &sync.WaitGroup{}
	var errs = make([]error, len(info.Parts))
	for index, part := range info.Parts {
		wg.Add(1)
		// Start deleting parts in routine.
		go func(index int, part MultipartPartInfo) {
			defer wg.Done()
			partFileName := partNumToPartFileName(part.PartNumber)
			errs[index] = xl.storage.DeleteFile(bucket, pathJoin(object, partFileName))
		}(index, part)
	}
	// Wait for all the deletes to finish.
	wg.Wait()
	// Loop through and validate if any errors, if we are unable to remove any part return
	// "unexpected" error as returning any other error might be misleading. For ex.
	// if DeleteFile() had returned errFileNotFound and we return it, then client would see
	// ObjectNotFound which is misleading.
	for _, err := range errs {
		if err != nil {
			return errUnexpected
		}
	}
	err = xl.storage.DeleteFile(bucket, pathJoin(object, multipartMetaFile))
	if err != nil {
		return err
	}
	return nil
}

// DeleteObject - delete the object.
func (xl xlObjects) DeleteObject(bucket, object string) error {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return BucketNameInvalid{Bucket: bucket}
	}
	if !isBucketExist(xl.storage, bucket) {
		return BucketNotFound{Bucket: bucket}
	}
	if !IsValidObjectName(object) {
		return ObjectNameInvalid{Bucket: bucket, Object: object}
	}
	if err := xl.deleteObject(bucket, object); err != nil {
		return toObjectErr(err, bucket, object)
	}
	return nil
}

// ListObjects - list all objects at prefix, delimited by '/'.
func (xl xlObjects) ListObjects(bucket, prefix, marker, delimiter string, maxKeys int) (ListObjectsInfo, error) {
	return listObjectsCommon(xl, bucket, prefix, marker, delimiter, maxKeys)
}
