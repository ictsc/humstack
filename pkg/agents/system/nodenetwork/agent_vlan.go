package nodenetwork

import (
	"fmt"
	"strconv"
	"time"

	"github.com/n0stack/n0stack/n0core/pkg/driver/iproute2"
	"github.com/ophum/humstack/pkg/agents/system/nodenetwork/utils"
	"github.com/ophum/humstack/pkg/agents/system/nodenetwork/vlan"
	"github.com/ophum/humstack/pkg/api/meta"
	"github.com/ophum/humstack/pkg/api/system"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

func (a *NodeNetworkAgent) syncVLANNetwork(network *system.NodeNetwork) error {

	bridgeName := utils.GenerateName("hum-br-", network.Group+network.Namespace+network.ID)
	vlanName := a.config.VLAN.DevName + "." + network.Spec.ID
	if a.config.VLAN.VLANInterfaceNamePrefix != "" {
		vlanName = a.config.VLAN.VLANInterfaceNamePrefix + "." + network.Spec.ID
	}

	// vlan idがすでに別のbridgeに接続されているかチェックする
	vlanLink, err := netlink.LinkByName(vlanName)
	if err != nil {
		if err.Error() != "Link not found" {
			return errors.Wrap(err, fmt.Sprintf("Failed to check existing vlan bridge using %s", vlanName))
		}
	}
	if err == nil {
		index := vlanLink.Attrs().MasterIndex
		attachedBr, err := netlink.LinkByIndex(index)
		if err != nil {
			if err.Error() != "Link not found" {
				return errors.Wrap(err, fmt.Sprintf("Failed to check existing vlan bridge member for %s : %d ", vlanName, index))
			}
		} else {
			if bridgeName != attachedBr.Attrs().Name {
				// vlan id is already used
				network.Status.Logs = append(network.Status.Logs, system.NodeNetworkStatusLog{
					NodeID:   a.node,
					Datetime: time.Now().String(),
					Log:      fmt.Sprintf("vlan id `%s` is already used.", network.Spec.ID),
				})
				if _, err := a.client.SystemV0().NodeNetwork().Update(network); err != nil {
					return errors.Wrap(err, fmt.Sprintf("Failed to Update nodenetwork api %s ", network.Name))
				}
				return fmt.Errorf("vlan id `%s` is already used.", network.Spec.ID)
			}
		}
	}

	// 作成だけ
	_, err = iproute2.NewBridge(bridgeName)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Failed to create vlan bridge %s",bridgeName))
	}

	id, err := strconv.ParseInt(network.Spec.ID, 10, 64)
	if err != nil {
		return errors.Wrap(err, "Failed to ParseInt() ")
	}
	dev, err := netlink.LinkByName(a.config.VLAN.DevName)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Failed to find interface %s ", a.config.VLAN.DevName))
	}

	vlan, err := vlan.NewVlan(dev, vlanName, int(id))
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Failed to split vlan %s interface %s", vlanName, dev))
	}

	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Failed to find bridge %s ", bridgeName))
	}

	// 削除処理
	if network.DeleteState == meta.DeleteStateDelete {
		if err := vlan.Delete(); err != nil {
			return errors.Wrap(err, "delete vlan")
		}

		if err := netlink.LinkDel(br); err != nil {
			return errors.Wrap(err, "delete bridge")
		}
		if err := a.client.SystemV0().NodeNetwork().Delete(network.Group, network.Namespace, network.ID); err != nil {
			return errors.Wrap(err, "delete node network")
		}
		return nil
	}

	err = vlan.SetMaster(br)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Failed to set master bridge %s ", br))
	}

	network.Annotations[NetworkV0AnnotationBridgeName] = bridgeName
	network.Annotations[NetworkV0AnnotationVLANName] = vlanName
	network.Status.State = system.NetworkStateAvailable

	return setHash(network)
}
