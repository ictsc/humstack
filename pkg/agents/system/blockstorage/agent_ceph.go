package blockstorage

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ceph/go-ceph/rados"
	"github.com/ceph/go-ceph/rbd"
	"github.com/ophum/humstack/pkg/api/meta"
	"github.com/ophum/humstack/pkg/api/system"
	"github.com/pkg/errors"
)

func getImageNameWithGroupAndNS(bs *system.BlockStorage) string {
	return filepath.Join(bs.Group, bs.Namespace, bs.ID)
}

func (a *BlockStorageAgent) syncCephBlockStorage(bs *system.BlockStorage) error {
	// ex. rbd:pool-name/image-name
	path := filepath.Join(fmt.Sprintf("rbd:%s", a.config.CephBackend.PoolName), bs.ID)
	imageNameWithGroupAndNS := getImageNameWithGroupAndNS(bs)

	// コピー中・ダウンロード中の場合はskip
	switch bs.Status.State {
	case system.BlockStorageStateCopying, system.BlockStorageStateDownloading:
		return nil
	}

	// 削除処理
	if bs.DeleteState == meta.DeleteStateDelete {
		return a.deleteCephBlockStorage(bs)
	}

	// イメージが存在するならsukip
	if a.cephImageIsExists(bs) {
		switch bs.Status.State {
		case system.BlockStorageStateError:
			// Stateがエラーなら存在するイメージは消す
			err := func() error {
				conn, err := a.newCephConn()
				if err != nil {

					return errors.Wrap(err, "Failed to newCephConn() ")
				}
				defer conn.Shutdown()

				ioctx, err := conn.OpenIOContext(a.config.CephBackend.PoolName)
				if err != nil {
					return errors.Wrap(err, "Failed to open ceph pool ")
				}
				defer ioctx.Destroy()

				return rbd.RemoveImage(ioctx, imageNameWithGroupAndNS)
			}()
			if err != nil {
				if err := a.setStateError(bs); err != nil {
					return err
				}
				return err
			}
		case "", system.BlockStorageStatePending:
			// 良くなさそう
			bs.Status.State = system.BlockStorageStateActive
			if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
				return errors.Wrap(err, "Failed to update bs ")
			}
			return setHash(bs)
		case system.BlockStorageStateActive, system.BlockStorageStateUsed:
			// イメージが存在しActive, Usedなので処理は不要
			if bs.Annotations == nil {
				bs.Annotations = map[string]string{}
			}

			_, poolOk := bs.Annotations["ceph-pool-name"]
			_, imageOk := bs.Annotations["ceph-image-name"]

			if !poolOk || !imageOk {
				bs.Annotations["ceph-pool-name"] = a.config.CephBackend.PoolName
				bs.Annotations["ceph-image-name"] = imageNameWithGroupAndNS
				if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
					return errors.Wrap(err, "Failed to update bs ")
				}
			}
			return nil
		}
	}

	if bs.Annotations == nil {
		bs.Annotations = map[string]string{}
	}

	bs.Annotations["ceph-pool-name"] = a.config.CephBackend.PoolName
	bs.Annotations["ceph-image-name"] = imageNameWithGroupAndNS
	if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
		return errors.Wrap(err, "Failed to update bs ")
	}

	switch bs.Spec.From.Type {
	case system.BlockStorageFromTypeEmpty:
		command := "qemu-img"
		args := []string{
			"create",
			"-f",
			"qcow2",
			path,
			withUnitToWithoutUnit(bs.Spec.LimitSize),
		}

		cmd := exec.Command(command, args...)
		if _, err := cmd.CombinedOutput(); err != nil {
			bs.Status.State = system.BlockStorageStateError
			if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
				return err
			}
			return err
		}
	case system.BlockStorageFromTypeHTTP:
		// TODO: From HTTP
		if err := a.setStateDownloading(bs); err != nil {
			return err
		}

		res, err := http.Get(bs.Spec.From.HTTP.URL)
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return err
		}
		defer res.Body.Close()

		conn, err := a.newCephConn()
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return errors.Wrap(err, "new ceph conn")
		}
		defer conn.Shutdown()

		// cephのpoolにイメージを作る
		ioctx, err := conn.OpenIOContext(a.config.CephBackend.PoolName)
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return errors.Wrap(err, "open io context")
		}
		defer ioctx.Destroy()

		size, err := strconv.ParseUint(withUnitToWithoutUnit(bs.Spec.LimitSize), 10, 64)
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return err
		}
		cephImage, err := rbd.Create(ioctx, imageNameWithGroupAndNS, size, 22)
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return err
		}

		if err := cephImage.Open(); err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return err
		}
		defer cephImage.Close()

		// BaseImageのデータをcephのimageに書き込む
		if res.ContentLength >= 0 {
			_, err = io.CopyN(cephImage, res.Body, res.ContentLength)
		} else {
			_, err = io.Copy(cephImage, res.Body)
		}
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return err
		}

		// リサイズ
		namespace := ""
		command := "qemu-img"
		args := []string{
			"resize",
			fmt.Sprintf("rbd:%s/%s/%s", a.config.CephBackend.PoolName, namespace, imageNameWithGroupAndNS),
			withUnitToWithoutUnit(bs.Spec.LimitSize),
		}
		cmd := exec.Command(command, args...)
		if stdoutstderr, err := cmd.CombinedOutput(); err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return errors.Wrap(err, fmt.Sprintf("qemu-img %s :%s", strings.Join(args, " "), stdoutstderr) );
		}

	case system.BlockStorageFromTypeBaseImage:
		// TODO: From BaseImage

		if err := a.setStateCopying(bs); err != nil {
			return err
		}

		image, err := a.client.SystemV0().Image().Get(bs.Group, bs.Spec.From.BaseImage.ImageName)
		if err != nil {
			bs.Status.State = system.BlockStorageStateError
			if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
				return err
			}
			return err
		}

		imageEntityID, ok := image.Spec.EntityMap[bs.Spec.From.BaseImage.Tag]
		if !ok {
			bs.Status.State = system.BlockStorageStateError
			if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
				return err
			}
			return fmt.Errorf("Image Entity not found")
		}
		imageEntity, err := a.client.SystemV0().ImageEntity().Get(bs.Group, imageEntityID)

		// cephのpoolにイメージを作る
		conn, err := a.newCephConn()
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return errors.Wrap(err, "Failed to newCephConn() ")
		}
		defer conn.Shutdown()

		ioctx, err := conn.OpenIOContext(a.config.CephBackend.PoolName)
		if err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return errors.Wrap(err, "Failed to open ceph pool ")
		}
		defer ioctx.Destroy()

		// imageEntityがlocalにある場合
		if imageEntity.Spec.Type == "Local" || imageEntity.Spec.Type == "" {
			log.Printf("Create ceph image from local image %s %s", bs.Spec.From.BaseImage.ImageName, bs.Spec.From.BaseImage.Tag)
			srcDirPath := filepath.Join(a.localImageDirectory, bs.Group)
			if !fileIsExists(srcDirPath) {
				if err := os.MkdirAll(srcDirPath, 0755); err != nil {
					bs.Status.State = system.BlockStorageStateError
					if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
						return err
					}
					return err
				}
			}
			srcPath := filepath.Join(srcDirPath, imageEntity.ID)

			// localになかったら別のノードから持ってくる
			// TODO: agent_localでも使っているので関数にする
			if !fileIsExists(srcPath) {
				log.Printf("fetch local image %s %s from another node", bs.Spec.From.BaseImage.ImageName, bs.Spec.From.BaseImage.Tag)
				err := func() error {
					src, err := os.Create(srcPath)
					if err != nil {
						return err
					}
					defer src.Close()

					stream, _, err := a.client.SystemV0().Image().Download(bs.Group, bs.Spec.From.BaseImage.ImageName, bs.Spec.From.BaseImage.Tag)
					if err != nil {
						return errors.Wrap(err, "Failed to download remote image")
					}
					defer stream.Close()

					if _, err := io.Copy(src, stream); err != nil {
						return err
					}
					return nil
				}()
				if err != nil {
					if err := a.setStateError(bs); err != nil {
						return err
					}
					return err
				}
			}

			src, err := os.Open(srcPath)
			if err != nil {
				if err := a.setStateError(bs); err != nil {
					return err
				}
				return err
			}
			defer src.Close()

			size, err := strconv.ParseUint(withUnitToWithoutUnit(bs.Spec.LimitSize), 10, 64)
			if err != nil {
				if err := a.setStateError(bs); err != nil {
					return err
				}
				return err
			}

			cephImage, err := rbd.Create(ioctx, imageNameWithGroupAndNS, size, 22)
			log.Printf("Init ceph rbd %s", imageNameWithGroupAndNS)
			if err != nil {
				if err := a.setStateError(bs); err != nil {
					return err
				}
				return err
			}
			defer cephImage.Close()

			if err := cephImage.Open(); err != nil {
				if err := a.setStateError(bs); err != nil {
					return err
				}
				return err
			}

			// BaseImageのデータをcephのimageに書き込む
			log.Printf("Upload image to ceph rbd %s", imageNameWithGroupAndNS)
			if finfo, err := src.Stat(); err == nil {
				_, err = io.CopyN(cephImage, src, finfo.Size())
			} else {
				_, err = io.Copy(cephImage, src)
			}
			if err != nil {
				if err := a.setStateError(bs); err != nil {
					return err
				}
				return err
			}

			// リサイズ
			namespace := ""
			command := "qemu-img"
			args := []string{
				"resize",
				fmt.Sprintf("rbd:%s/%s/%s", a.config.CephBackend.PoolName, namespace, imageNameWithGroupAndNS),
				withUnitToWithoutUnit(bs.Spec.LimitSize),
			}
			cmd := exec.Command(command, args...)
			if stdoutstderr, err := cmd.CombinedOutput(); err != nil {
				if err := a.setStateError(bs); err != nil {
					return err
				}
				return errors.Wrap(err, fmt.Sprintf("qemu-img %s :%s", strings.Join(args, " "), stdoutstderr) );
			}
		} else if imageEntity.Spec.Type == "Ceph" {
			snapName := imageEntity.Annotations["imageentityv0/ceph-snapname"]
			imageName := imageEntity.Annotations["imageentityv0/ceph-imagename"]
			cephImage, err := rbd.OpenImageReadOnly(ioctx, imageName, rbd.NoSnapshot)
			if err != nil {
				return errors.Wrapf(err, "Failed to open image read only `%s`", imageName)
			}
			defer cephImage.Close()
			if err := rbd.CloneFromImage(cephImage, snapName, ioctx, imageNameWithGroupAndNS, rbd.NewRbdImageOptions()); err != nil {
				return errors.Wrapf(err, "Failed to clone from image `%s` from `%s@%s`", imageNameWithGroupAndNS, cephImage, snapName)
			}
			// ceph image のリサイズ
			if image, err := rbd.OpenImage(ioctx, imageNameWithGroupAndNS, ""); err != nil {
				return errors.Wrapf(err, "Failed to open image `%s`", imageNameWithGroupAndNS)
			} else {
				defer image.Close()
				size, err := strconv.ParseUint(withUnitToWithoutUnit(bs.Spec.LimitSize), 10, 64)
				if err != nil {
					if err := a.setStateError(bs); err != nil {
						return err
					}
					return errors.Wrapf(err, "Failed to parse uint limit size`%s`", bs.Spec.LimitSize)
				}
				if err := image.Resize(size); err != nil {
					return errors.Wrapf(err, "Failed to resize rbd image `%s`", bs.ID)
				}
			}
			// ceph image内のqcow2リサイズ
			namespace := ""
			command := "qemu-img"
			args := []string{
				"resize",
				fmt.Sprintf("rbd:%s/%s/%s", a.config.CephBackend.PoolName, namespace, imageNameWithGroupAndNS),
				withUnitToWithoutUnit(bs.Spec.LimitSize),
			}
			cmd := exec.Command(command, args...)
			if stdoutstderr, err := cmd.CombinedOutput(); err != nil {
				if err := a.setStateError(bs); err != nil {
					return errors.Wrapf(err, "Failed to exec `%s %s` but cannot update state", command, strings.Join(args, " "))
				}
				return errors.Wrap(err, fmt.Sprintf("qemu-img %s :%s", strings.Join(args, " "), stdoutstderr) );
			}

		}
	}

	if bs.Status.State == "" ||
		bs.Status.State == system.BlockStorageStatePending ||
		bs.Status.State == system.BlockStorageStateCopying ||
		bs.Status.State == system.BlockStorageStateDownloading {
		bs.Status.State = system.BlockStorageStateActive

		if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
			return err
		}
	}
	return setHash(bs)
}

func (a *BlockStorageAgent) deleteCephBlockStorage(bs *system.BlockStorage) error {
	imageNameWithGroupAndNS := filepath.Join(bs.Group, bs.Namespace, bs.ID)
	if bs.Status.State != "" &&
		bs.Status.State != system.BlockStorageStateError &&
		bs.Status.State != system.BlockStorageStateQueued &&
		bs.Status.State != system.BlockStorageStateActive {
		return nil
	}

	bs.Status.State = system.BlockStorageStateDeleting
	_, err := a.client.SystemV0().BlockStorage().Update(bs)
	if err != nil {
		return err
	}

	// ceph からイメージを消す
	conn, err := a.newCephConn()
	if err != nil {
		if err := a.setStateError(bs); err != nil {
			return err
		}
		return err
	}
	defer conn.Shutdown()

	ioctx, err := conn.OpenIOContext(a.config.CephBackend.PoolName)
	if err != nil {
		if err := a.setStateError(bs); err != nil {
			return err
		}
		return err
	}
	defer ioctx.Destroy()

	if a.cephImageIsExists(bs) {
		if err := rbd.RemoveImage(ioctx, imageNameWithGroupAndNS); err != nil {
			if err := a.setStateError(bs); err != nil {
				return err
			}
			return err
		}
	}

	err = a.client.SystemV0().BlockStorage().Delete(bs.Group, bs.Namespace, bs.ID)
	if err != nil {
		return err
	}
	return nil
}

func (a BlockStorageAgent) cephImageIsExists(bs *system.BlockStorage) bool {
	// typeがCephでない
	if t, ok := bs.Annotations[BlockStorageV0AnnotationType]; ok && t != BlockStorageV0BlockStorageTypeCeph {
		return false
	}

	imageName := getImageNameWithGroupAndNS(bs)

	conn, err := a.newCephConn()
	if err != nil {
		return false
	}
	defer conn.Shutdown()

	ioctx, err := conn.OpenIOContext(a.config.CephBackend.PoolName)
	if err != nil {
		return false
	}
	defer ioctx.Destroy()

	if image, err := rbd.OpenImageReadOnly(ioctx, imageName, ""); err != nil {
		return false
	} else {
		defer image.Close()
	}
	return true
}

func (a BlockStorageAgent) newCephConn() (*rados.Conn, error) {
	// ceph の設定がある場合はコネクションを張る
	cephConn, err := rados.NewConn()
	if err != nil {
		log.Fatal(err)
	}

	if err := cephConn.ReadConfigFile(a.config.CephBackend.ConfigPath); err != nil {
		return nil, errors.Wrap(err, "read config file")
	}

	if err := cephConn.Connect(); err != nil {
		return nil, errors.Wrap(err, "connect")
	}
	return cephConn, nil
}

func (a BlockStorageAgent) setStateError(bs *system.BlockStorage) error {
	bs.Status.State = system.BlockStorageStateError
	if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
		return err
	}
	return nil
}

func (a BlockStorageAgent) setStateCopying(bs *system.BlockStorage) error {
	bs.Status.State = system.BlockStorageStateCopying
	if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
		return err
	}
	return nil
}

func (a BlockStorageAgent) setStateDownloading(bs *system.BlockStorage) error {
	bs.Status.State = system.BlockStorageStateDownloading
	if _, err := a.client.SystemV0().BlockStorage().Update(bs); err != nil {
		return err
	}
	return nil
}
