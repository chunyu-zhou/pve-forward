package server

import (
	"fmt"
	"strings"

	"pveforward/internal/ports"
	"pveforward/internal/store"
)

// parseNodePorts 校验节点的可分配端口范围与保留端口（均支持单个端口和范围，如 "22,8006,3000-3100"）。
func parseNodePorts(rangeSpec, reservedSpec string) (store.NodePorts, error) {
	rng, err := ports.ParseSet(rangeSpec)
	if err != nil {
		return store.NodePorts{}, bad("可分配端口范围格式错误：%v", err)
	}
	if rng.Empty() {
		return store.NodePorts{}, bad("请填写可分配端口范围，如 10000-60000")
	}
	res, err := ports.ParseSet(reservedSpec)
	if err != nil {
		return store.NodePorts{}, bad("保留端口格式错误：%v", err)
	}
	if rng.CountExcept(res) == 0 {
		return store.NodePorts{}, bad("可分配端口范围内的端口都被保留了")
	}
	return store.NodePorts{Range: rng.String(), Reserved: res.String()}, nil
}

// checkNodePortsChange 修改端口策略前检查已有的端口分配：不能有分配落在新范围之外或成为保留端口。
func (s *Server) checkNodePortsChange(nodeID int64, np store.NodePorts) error {
	rows, err := s.db.Query(`SELECT a.port, u.username FROM allocated_ports a JOIN users u ON u.id=a.user_id WHERE a.node_id=? ORDER BY a.port`, nodeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var list []string
	count := 0
	for rows.Next() {
		var p int
		var user string
		if err := rows.Scan(&p, &user); err != nil {
			return err
		}
		if np.Allowed(p) {
			continue
		}
		count++
		if len(list) < 5 {
			list = append(list, fmt.Sprintf("%s 的 %d", user, p))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count > 0 {
		more := ""
		if count > len(list) {
			more = fmt.Sprintf(" 等 %d 个", count)
		}
		return userError{fmt.Sprintf("已分配的端口（%s%s）不在新的可分配范围内或被设为保留端口，请先在用户管理中调整这些用户的端口分配", strings.Join(list, "、"), more)}
	}
	return nil
}
