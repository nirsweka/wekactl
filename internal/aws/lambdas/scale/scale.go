package scale

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"math/rand"
	"sort"
	"time"
	"wekactl/internal/aws/lambdas/protocol"
	"wekactl/internal/connectors"
	"wekactl/internal/lib/jrpc"
	"wekactl/internal/lib/math"
	"wekactl/internal/lib/strings"
	"wekactl/internal/lib/types"
	"wekactl/internal/lib/weka"
)

type hostState int

const unhealthyDeactivateTimeout = 120 * time.Minute
const backendCleanupDelay = 5*time.Minute // Giving own HG chance to take care
const downKickOutTimeout = 3 * time.Hour

func (h hostState) String() string {
	switch h {
	case DEACTIVATING:
		return "DEACTIVATING"
	case HEALTHY:
		return "HEALTHY"
	case UNHEALTHY:
		return "UNHEALTHY"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", h)
	}
}

const (
	/*
		Order matters, it defines priority of hosts removal
	*/
	DEACTIVATING hostState = iota
	UNHEALTHY
	HEALTHY
)

type driveMap map[weka.DriveId]weka.Drive
type nodeMap map[weka.NodeId]weka.Node
type hostInfo struct {
	weka.Host
	id         weka.HostId
	drives     driveMap
	nodes      nodeMap
	scaleState hostState
}

func (host hostInfo) belongsToHg(instances []protocol.HgInstance) bool {
	for _, instance := range instances {
		if host.Aws.InstanceId == instance.Id {
			return true
		}
	}
	return false
}

func (host hostInfo) belongsToHgIpBased(instances []protocol.HgInstance) bool {
	for _, instance := range instances {
		if host.HostIp == instance.PrivateIp {
			return true
		}
	}
	return false
}

func (host hostInfo) numNotHealthyDrives() int {
	notActive := 0
	for _, drive := range host.drives {
		if strings.AnyOf(drive.Status, "INACTIVE") {
			notActive += 1
		}
	}
	return notActive
}

func (host hostInfo) allDisksBeingRemoved() bool {
	ret := false
	for _, drive := range host.drives {
		ret = true
		if drive.ShouldBeActive {
			return false
		}
	}
	return ret
}

func (host hostInfo) anyDiskBeingRemoved() bool {
	for _, drive := range host.drives {
		if !drive.ShouldBeActive {
			return true
		}
	}
	return false
}

func (host hostInfo) allDrivesInactive() bool {
	for _, drive := range host.drives {
		if drive.Status != "INACTIVE" {
			return false
		}
	}
	return true
}

func (host hostInfo) managementTimedOut(timeout time.Duration) bool {
	for nodeId, node := range host.nodes {
		if !nodeId.IsManagement() {
			continue
		}
		if node.Status == "DOWN" && time.Since(*node.LastFencingTime) > timeout {
			return true
		}
	}
	return false
}


func Handler(ctx context.Context, info protocol.HostGroupInfoResponse) (response protocol.ScaleResponse, err error) {
	/*
		Code in here based on following logic:

		A - Fully active, healthy
		T - Desired target number
		U - Unhealthy, we want to remove it for whatever reason. DOWN host, FAILED drive, so on
		D - Drives/hosts being deactivated
		NEW_D - Decision to start deactivating, i.e transition to D, basing on U. Never more then 2 for U

		NEW_D = func(A, U, T, D)

		NEW_D = max(A+U+D-T, min(2-D, U), 0)
	*/
	jrpcBuilder := func(ip string) *jrpc.BaseClient {
		return connectors.NewJrpcClient(ctx, ip, weka.ManagementJrpcPort, info.Username, info.Password)
	}
	ips := info.BackendIps
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
	jpool := &jrpc.Pool{
		Ips:     ips,
		Clients: map[string]*jrpc.BaseClient{},
		Active:  "",
		Builder: jrpcBuilder,
		Ctx:     ctx,
	}

	systemStatus := weka.StatusResponse{}
	hostsApiList := weka.HostListResponse{}
	driveApiList := weka.DriveListResponse{}
	nodeApiList := weka.NodeListResponse{}

	err = jpool.Call(weka.JrpcStatus, struct{}{}, &systemStatus)
	if err != nil {
		return
	}
	err = isAllowedToScale(systemStatus)
	if err != nil {
		return
	}
	err = jpool.Call(weka.JrpcHostList, struct{}{}, &hostsApiList)
	if err != nil {
		return
	}
	if info.Role == "backend" {
		err = jpool.Call(weka.JrpcDrivesList, struct{}{}, &driveApiList)
		if err != nil {
			return
		}
	}
	err = jpool.Call(weka.JrpcNodeList, struct{}{}, &nodeApiList)
	if err != nil {
		return
	}

	hosts := map[weka.HostId]hostInfo{}
	for hostId, host := range hostsApiList {
		hosts[hostId] = hostInfo{
			Host:   host,
			id:     hostId,
			drives: driveMap{},
			nodes:  nodeMap{},
		}
	}
	for driveId, drive := range driveApiList {
		if _, ok := hosts[drive.HostId]; ok {
			hosts[drive.HostId].drives[driveId] = drive
		}
	}

	for nodeId, node := range nodeApiList {
		if _, ok := hosts[node.HostId]; ok {
			hosts[node.HostId].nodes[nodeId] = node
		}
	}

	var hostsList []hostInfo
	var inactiveHosts []hostInfo
	var downHosts []hostInfo

	for _, host := range hosts {
		switch host.State {
		case "INACTIVE":
			if host.belongsToHgIpBased(info.Instances) {
				inactiveHosts = append(inactiveHosts, host)
			} else {
				if info.Role == "backend" && time.Since(host.StateChangedTime) > backendCleanupDelay{
					// Since terminate logic is mostly delta based, and remove might be transient errors
					// We might have leftovers, that we are unable to recognize
					// So decision is, to kick out whatever is inactive.
					inactiveHosts = append(inactiveHosts, host)
				}
			}
		default:
			if host.belongsToHg(info.Instances) {
				hostsList = append(hostsList, host)
			} else if host.Status == "DOWN" {
				log.Info().Msgf("found down host, %s : %s : %s", host.id, host.Status, host.HostIp)
				if host.belongsToHgIpBased(info.Instances) {
					log.Info().Msgf("including in known hosts  %s : %s", host.id, host.Status)
					hostsList = append(hostsList, host)
				}
				// Down hosts lose instanceIds, so have to account basing on IPs
			}
		}

		switch host.Status {
		case "DOWN":
			if info.Role == "backend" {
				if host.State != "INACTIVE" && host.managementTimedOut(downKickOutTimeout) {
					log.Info().Msgf("host %s is still active but down for too long, kicking out", host.id)
					downHosts = append(downHosts, host)
				}
			}
		}
	}

	calculateHostsState(hostsList)

	sort.Slice(hostsList, func(i, j int) bool {
		// Giving priority to disks to hosts with disk being removed
		// Then hosts with disks not in active state
		// Then hosts sorted by add time
		a := hostsList[i]
		b := hostsList[j]
		if a.scaleState < b.scaleState {
			return true
		}
		if a.scaleState > b.scaleState {
			return false
		}
		if a.numNotHealthyDrives() > b.numNotHealthyDrives() {
			return true
		}
		if a.numNotHealthyDrives() < b.numNotHealthyDrives() {
			return false
		}
		return a.AddedTime.Before(b.AddedTime)
	})

	removeInactive(inactiveHosts, jpool, info.Instances, &response)
	removeOldDrives(driveApiList, jpool, &response)
	numToDeactivate := getNumToDeactivate(hostsList, info.DesiredCapacity)

	deactivateHost := func(host hostInfo) {
		log.Info().Msgf("Trying to deactivate host %s", host.id)
		for _, drive := range host.drives {
			if drive.ShouldBeActive {
				err := jpool.Call(weka.JrpcDeactivateDrives, types.JsonDict{
					"drive_uuids": []uuid.UUID{drive.Uuid},
				}, nil)
				if err != nil {
					log.Error().Err(err)
					response.AddTransientError(err, "deactivateDrive")
				}
			}
		}

		if host.allDrivesInactive() {
			jpool.Drop(host.HostIp)
			err := jpool.Call(weka.JrpcDeactivateHosts, types.JsonDict{
				"host_ids":                 []weka.HostId{host.id},
				"skip_resource_validation": false,
			}, nil)
			if err != nil {
				log.Error().Err(err)
				response.AddTransientError(err, "deactivateHost")
			}
		}

	}

	for _, host := range hostsList[:numToDeactivate] {
		deactivateHost(host)
	}

	for _, host := range downHosts {
		deactivateHost(host)
	}

	for _, host := range hostsList {
		response.Hosts = append(response.Hosts, protocol.ScaleResponseHost{
			InstanceId: host.Aws.InstanceId,
			State:      host.State,
			AddedTime:  host.AddedTime,
			HostId:     host.id,
		})
	}
	return
}

func remoteDownHosts(hosts []hostInfo, jpool *jrpc.Pool) {

}

func getNumToDeactivate(hostInfo []hostInfo, desired int) int {
	/*
		A - Fully active, healthy
		T - Target state
		U - Unhealthy, we want to remove it for whatever reason. DOWN host, FAILED drive, so on
		D - Drives/hosts being deactivated
		new_D - Decision to start deactivating, i.e transition to D, basing on U. Never more then 2 for U

		new_D = func(A, U, T, D)

		new_D = max(A+U+D-T, min(2-D, U), 0)
	*/

	nHealthy := 0
	nUnhealthy := 0
	nDeactivating := 0

	for _, host := range hostInfo {
		switch host.scaleState {
		case HEALTHY:
			nHealthy++
		case UNHEALTHY:
			nUnhealthy++
		case DEACTIVATING:
			nDeactivating++
		}
	}

	toDeactivate := calculateDeactivateTarget(nHealthy, nUnhealthy, nDeactivating, desired)
	log.Info().Msgf("%d hosts set to deactivate. nHealthy: %d nUnhealthy:%d nDeactivating: %d desired:%d", toDeactivate, nHealthy, nUnhealthy, nDeactivating, desired)
	return toDeactivate
}

func calculateDeactivateTarget(nHealthy int, nUnhealthy int, nDeactivating int, desired int) int {
	ret := math.Max(nHealthy+nUnhealthy+nDeactivating-desired, math.Min(2-nDeactivating, nUnhealthy))
	ret = math.Max(nDeactivating, ret)
	return ret
}

func isAllowedToScale(status weka.StatusResponse) error {
	if status.IoStatus != "STARTED" {
		return errors.New(fmt.Sprintf("io status:%s, aborting scale", status.IoStatus))
	}

	if status.Upgrade != "" {
		return errors.New("upgrade is running, aborting scale")
	}
	return nil
}

func deriveHostState(host *hostInfo) hostState {
	if host.allDisksBeingRemoved() {
		log.Info().Msgf("Marking %s as deactivating due to unhealthy disks", host.id.String())
		return DEACTIVATING
	}
	if strings.AnyOf(host.State, "DEACTIVATING", "REMOVING", "INACTIVE") {
		return DEACTIVATING
	}
	if host.Status == "DOWN" && host.managementTimedOut(unhealthyDeactivateTimeout) {
		log.Info().Msgf("Marking %s as unhealthy due to DOWN", host.id.String())
		return UNHEALTHY
	}
	if host.numNotHealthyDrives() > 0 || host.anyDiskBeingRemoved() {
		log.Info().Msgf("Marking %s as unhealthy due to unhealthy drives", host.id.String())
		return UNHEALTHY
	}
	return HEALTHY
}

func calculateHostsState(hosts []hostInfo) {
	for i := range hosts {
		host := &hosts[i]
		host.scaleState = deriveHostState(host)
	}
}

func selectInstanceByIp(ip string, instances []protocol.HgInstance) *protocol.HgInstance {
	for _, i := range instances {
		if i.PrivateIp == ip {
			return &i
		}
	}
	return nil
}

func removeInactive(hosts []hostInfo, jpool *jrpc.Pool, instances []protocol.HgInstance, p *protocol.ScaleResponse) {
	for _, host := range hosts {
		jpool.Drop(host.HostIp)
		err := jpool.Call(weka.JrpcRemoveHost, types.JsonDict{
			"host_id": host.id.Int(),
			"no_wait": true,
		}, nil)
		if err != nil {
			log.Error().Err(err)
			p.AddTransientError(err, "removeInactive")
			continue
		}
		instance := selectInstanceByIp(host.HostIp, instances)
		if instance != nil {
			p.ToTerminate = append(p.ToTerminate, *instance)
		}

		for _, drive := range host.drives {
			removeDrive(jpool, drive, p)
		}
	}
	return
}

func removeOldDrives(drives weka.DriveListResponse, jpool *jrpc.Pool, p *protocol.ScaleResponse) {
	for _, drive := range drives {
		if drive.HostId.Int() == -1 && drive.Status == "INACTIVE" {
			removeDrive(jpool, drive, p)
		}
	}
}

func removeDrive(jpool *jrpc.Pool, drive weka.Drive, p *protocol.ScaleResponse) {
	err := jpool.Call(weka.JrpcRemoveDrive, types.JsonDict{
		"drive_uuids": []uuid.UUID{drive.Uuid},
	}, nil)
	if err != nil {
		log.Error().Err(err)
		p.AddTransientError(err, "removeDrive")
	}
}
