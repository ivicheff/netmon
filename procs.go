package main

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

type tcpConn struct {
	pid         uint32
	remote      net.IP
	rport       uint16
	established bool
}

type agg struct {
	name          string
	via, dir, udp int
}

func numCell(n int, col string) string {
	if n == 0 {
		return fmt.Sprintf("%s%14s%s", gray, "-", reset)
	}
	return fmt.Sprintf("%s%14d%s", col, n, reset)
}

// Окно со списком программ: у кого сколько соединений и каким путём они идут.
func runProcs() {
	proxyPort := 0
	proxyText := "прокси не найден"
	if proxyURL != nil {
		proxyText = proxyURL.Host
		if _, p, err := net.SplitHostPort(proxyURL.Host); err == nil {
			proxyPort, _ = strconv.Atoi(p)
		}
	}

	for ; ; time.Sleep(2 * time.Second) {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%sПрограммы и сетевые соединения%s  (обновление каждые 2 с, Ctrl+C — выход)\n\n", cyan, reset))

		tcp, udp, names, err := collect()
		if err != nil {
			sb.WriteString(fmt.Sprintf("%sНе удалось получить список соединений: %v%s\n", red, err, reset))
			fmt.Print("\033[H\033[2J" + sb.String())
			continue
		}

		m := map[string]*agg{}
		get := func(pid uint32) *agg {
			n := names[pid]
			if n == "" {
				n = fmt.Sprintf("PID %d", pid)
			}
			a := m[n]
			if a == nil {
				a = &agg{name: n}
				m[n] = a
			}
			return a
		}
		for _, c := range tcp {
			if !c.established || c.remote.IsUnspecified() {
				continue
			}
			if c.remote.IsLoopback() {
				if proxyPort != 0 && int(c.rport) == proxyPort {
					get(c.pid).via++ // соединение с локальным прокси
				}
				continue
			}
			get(c.pid).dir++
		}
		for _, pid := range udp {
			get(pid).udp++
		}

		rows := make([]*agg, 0, len(m))
		for _, a := range m {
			rows = append(rows, a)
		}
		sort.Slice(rows, func(i, j int) bool {
			ti, tj := rows[i].via+rows[i].dir+rows[i].udp, rows[j].via+rows[j].dir+rows[j].udp
			if ti != tj {
				return ti > tj
			}
			return rows[i].name < rows[j].name
		})

		sb.WriteString(fmt.Sprintf("%-30s %14s %14s %14s\n", "Программа", "Через прокси", "Напрямую", "UDP-сокеты"))
		sb.WriteString(strings.Repeat("─", 76) + "\n")
		for i, a := range rows {
			if i >= 30 {
				break
			}
			sb.WriteString(fmt.Sprintf("%-30s %s %s %s\n", a.name,
				numCell(a.via, green), numCell(a.dir, yellow), numCell(a.udp, cyan)))
		}

		sb.WriteString(gray + "\n")
		sb.WriteString(fmt.Sprintf("Через прокси — TCP-соединения программы с %s (то есть через Xray / системный прокси).\n", proxyText))
		sb.WriteString("Напрямую — TCP-соединения с интернетом мимо прокси. Если у программы тут есть числа, а в первом\n")
		sb.WriteString("  столбце пусто — она не использует системный прокси. У самого Xray/v2ray это канал до вашего сервера.\n")
		sb.WriteString("UDP-сокеты — открытые UDP-порты. Системный прокси UDP не пропускает, поэтому голос, звонки и игры\n")
		sb.WriteString("  идут напрямую.\n")
		sb.WriteString("Это число соединений, а не объём трафика. Объём по программам: Win+R → resmon → вкладка «Сеть».\n")
		sb.WriteString(reset)
		fmt.Print("\033[H\033[2J" + sb.String())
	}
}
