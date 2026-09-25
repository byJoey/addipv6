#!/bin/bash
# ───────────────────────────────────────────────────────────────
# 这是最早的 shell 版，留在这里是为了不让老的一键链接失效。
# 新版已用 Go 重写，带网页界面，还能把地址批量解析到 Cloudflare：
#   bash <(curl -fsSL https://raw.githubusercontent.com/byJoey/addipv6/main/install.sh)
# 新老互不影响，这个脚本本身照常能用。
# ───────────────────────────────────────────────────────────────

echo "====================================="
echo "欢迎使用 ADD IPv6 管理工具"
echo "作者: Joey"
echo "博客: joeyblog.net"
echo "TG群: https://t.me/+ft-zI76oovgwNmRh"
echo "提醒: 合理使用"
echo "====================================="

# 1. 必须以 root 权限执行
if [ "$(id -u)" -ne 0 ]; then
    echo "请以root权限执行此脚本。"
    exit 1
fi

# 2. 用来选择具有全局 IPv6 的网卡
function choose_interface() {
    GLOBAL_IPV6_INTERFACES=()
    for iface in $(ip -o link show | awk -F': ' '{print $2}'); do
        if ip -6 addr show dev "$iface" scope global | grep -q 'inet6'; then
            GLOBAL_IPV6_INTERFACES+=("$iface")
        fi
    done
    if [ ${#GLOBAL_IPV6_INTERFACES[@]} -eq 0 ]; then
        echo "未检测到具有全局 IPv6 地址的网卡，请检查VPS的网络配置。"
        exit 1
    fi
    if [ ${#GLOBAL_IPV6_INTERFACES[@]} -eq 1 ]; then
        SELECTED_IFACE="${GLOBAL_IPV6_INTERFACES[0]}"
    else
        echo "检测到以下具有全局 IPv6 地址的网卡："
        for i in "${!GLOBAL_IPV6_INTERFACES[@]}"; do
            echo "$((i+1)). ${GLOBAL_IPV6_INTERFACES[$i]}"
        done
        read -p "请选择要使用的网卡编号: " choice
        if ! [[ "$choice" =~ ^[1-9][0-9]*$ ]] || [ "$choice" -gt "${#GLOBAL_IPV6_INTERFACES[@]}" ]; then
            echo "选择无效。"
            exit 1
        fi
        SELECTED_IFACE="${GLOBAL_IPV6_INTERFACES[$((choice-1))]}"
    fi
    echo "选择的网卡为：$SELECTED_IFACE"
}

# 3. 添加随机 IPv6 地址函数
function add_random_ipv6() {
    # 检查 python3 依赖
    if ! command -v python3 &> /dev/null; then
        echo "本功能需要 python3 来随机生成 IPv6 地址，请先安装 python3 后再试。"
        exit 1
    fi

    choose_interface
    # 获取网卡上任意一个全局 IPv6 地址(含CIDR)
    ipv6_cidr=$(ip -6 addr show dev "$SELECTED_IFACE" scope global | awk '/inet6/ {print $2}' | head -n1)
    if [ -z "$ipv6_cidr" ]; then
        echo "无法获取 $SELECTED_IFACE 的全局 IPv6 地址。"
        exit 1
    fi

    BASE_ADDR=$(echo "$ipv6_cidr" | cut -d'/' -f1)
    PLEN=$(echo "$ipv6_cidr" | cut -d'/' -f2)
    echo "检测到 IPv6 地址: $ipv6_cidr"
    echo "使用的网络: $BASE_ADDR/$PLEN"

    # 如果是 /128，就无法再随机生成其他地址
    if [ "$PLEN" -eq 128 ]; then
        echo "检测到前缀为 /128，不支持随机生成其它地址。"
        exit 1
    fi

    read -p "请输入要添加的随机 IPv6 地址数量: " COUNT
    if ! [[ "$COUNT" =~ ^[0-9]+$ ]]; then
        echo "数量必须为整数。"
        exit 1
    fi

    # 执行循环随机添加
    for (( i=1; i<=COUNT; i++ )); do
        RANDOM_ADDR=$(python3 -c "import ipaddress, random; \
net=ipaddress.ip_network('$BASE_ADDR/$PLEN', strict=False); \
print(ipaddress.IPv6Address(random.randint(int(net.network_address), int(net.broadcast_address))))")
        
        echo "添加地址: ${RANDOM_ADDR}/${PLEN}"
        ip -6 addr add "${RANDOM_ADDR}/${PLEN}" dev "$SELECTED_IFACE"
        if [ $? -eq 0 ]; then
            # 记录添加成功的地址
            echo "${RANDOM_ADDR}/${PLEN}" >> /tmp/added_v6_ipv6.txt
        else
            echo "添加 ${RANDOM_ADDR}/${PLEN} 失败，请检查系统日志或网络配置。"
        fi
    done
    echo "所有随机地址添加完成。"
}

# 4. 管理默认出口 IPv6 地址
function manage_default_ipv6() {
    choose_interface

    # 列出所有全局 IPv6 地址
    echo "检测到以下IPv6地址（全局范围）："
    mapfile -t ipv6_list < <(ip -6 addr show dev "$SELECTED_IFACE" scope global | awk '/inet6/ {print $2}')
    if [ ${#ipv6_list[@]} -eq 0 ]; then
        echo "网卡 $SELECTED_IFACE 上未检测到全局IPv6地址。"
        exit 1
    fi

    for i in "${!ipv6_list[@]}"; do
        echo "$((i+1)). ${ipv6_list[$i]}"
    done

    read -p "请输入要设置为出口的IPv6地址对应的序号: " addr_choice
    if ! [[ "$addr_choice" =~ ^[0-9]+$ ]] || [ "$addr_choice" -gt "${#ipv6_list[@]}" ] || [ "$addr_choice" -lt 1 ]; then
        echo "选择无效。"
        exit 1
    fi

    # 拿到选中的地址（去掉 /前缀长度）
    SELECTED_ENTRY="${ipv6_list[$((addr_choice-1))]}"
    SELECTED_IP=$(echo "$SELECTED_ENTRY" | cut -d'/' -f1)
    echo "选择的默认出口IPv6地址为：$SELECTED_IP"

    # 尝试检测默认网关
    GATEWAY=$(ip -6 route show default dev "$SELECTED_IFACE" | awk '/default/ {print $3}' | head -n1)
    # 如果没有 default dev XXX，再去找所有 dev XXX 下的 via
    if [ -z "$GATEWAY" ]; then
        GATEWAY=$(ip -6 route show dev "$SELECTED_IFACE" | awk '/via/ {print $3}' | head -n1)
    fi
    if [ -z "$GATEWAY" ]; then
        echo "未检测到默认IPv6网关，请检查系统路由配置。"
        exit 1
    fi

    echo "检测到默认IPv6网关为：$GATEWAY"

    # 优先尝试 'add default'；若已存在，改用 'change default'
    ip -6 route add default via "$GATEWAY" dev "$SELECTED_IFACE" src "$SELECTED_IP" onlink 2>/tmp/ip6_err.log || \
    ip -6 route change default via "$GATEWAY" dev "$SELECTED_IFACE" src "$SELECTED_IP" onlink 2>>/tmp/ip6_err.log

    if [ $? -eq 0 ]; then
        echo "默认出口IPv6地址更新成功，出站流量将使用 $SELECTED_IP 作为源地址。"
    else
        echo "更新默认出口IPv6地址失败，请检查系统路由配置（错误详情可查看 /tmp/ip6_err.log）。"
    fi

    # 询问是否持久化到 /etc/rc.local
    read -p "是否将此配置写入 /etc/rc.local 以避免重启后失效？(y/n): " persist_choice
    if [[ "$persist_choice" =~ ^[Yy]$ ]]; then
        if [ ! -f /etc/rc.local ]; then
            echo "#!/bin/bash" > /etc/rc.local
            chmod +x /etc/rc.local
        fi
        # 避免重复追加
        local_cmd="ip -6 route add default via \"$GATEWAY\" dev \"$SELECTED_IFACE\" src \"$SELECTED_IP\" onlink"
        if ! grep -Fxq "$local_cmd" /etc/rc.local; then
            echo "$local_cmd" >> /etc/rc.local
            echo "配置已写入 /etc/rc.local 。"
        else
            echo "检测到 /etc/rc.local 中已存在相同配置，无需重复写入。"
        fi
    fi
}

# 5. 一键删除脚本添加过的所有 IPv6 地址
function delete_all_ipv6() {
    choose_interface
    if [ ! -f /tmp/added_v6_ipv6.txt ]; then
        echo "未检测到 /tmp/added_v6_ipv6.txt 文件，说明没有记录或已被删除。"
        exit 1
    fi

    while read -r entry; do
        if [ -n "$entry" ]; then
            echo "删除地址: $entry"
            ip -6 addr del "$entry" dev "$SELECTED_IFACE"
        fi
    done < /tmp/added_v6_ipv6.txt

    rm -f /tmp/added_v6_ipv6.txt
    echo "所有添加的IPv6地址已删除。"
}

# 6. 只保留当前默认出口IPv6地址，删除其它所有
function delete_except_default_ipv6() {
    choose_interface

    # 获取当前默认路由的源地址
    default_ip=$(ip -6 route show default dev "$SELECTED_IFACE" | \
        awk '{for(i=1;i<=NF;i++){if($i=="src"){print $(i+1); exit}}}')

    if [ -z "$default_ip" ]; then
        echo "未检测到默认出口IPv6地址，请先设置默认出口IPv6地址后再执行。"
        exit 1
    fi

    echo "当前默认出口IPv6地址: $default_ip"

    # 遍历所有全局IPv6地址，删除与默认出口地址不同的
    mapfile -t ip_entries < <(ip -6 addr show dev "$SELECTED_IFACE" scope global | awk '/inet6/ {print $2}')
    for entry in "${ip_entries[@]}"; do
        addr_only=$(echo "$entry" | cut -d'/' -f1)
        if [ "$addr_only" != "$default_ip" ]; then
            echo "删除地址: $entry"
            ip -6 addr del "$entry" dev "$SELECTED_IFACE"
        fi
    done

    echo "只保留默认出口IPv6地址 $default_ip，其它已删除。"

    # 询问是否将当前配置写入 /etc/rc.local
    read -p "是否将当前配置写入 /etc/rc.local 以避免重启后失效？(y/n): " persist_choice
    if [[ "$persist_choice" =~ ^[Yy]$ ]]; then
        gateway=$(ip -6 route show default dev "$SELECTED_IFACE" | awk '/default/ {print $3}' | head -n1)
        if [ -z "$gateway" ]; then
            echo "未检测到默认IPv6网关，无法写入配置。"
        else
            if [ ! -f /etc/rc.local ]; then
                echo "#!/bin/bash" > /etc/rc.local
                chmod +x /etc/rc.local
            fi
            local_cmd="ip -6 route add default via \"$gateway\" dev \"$SELECTED_IFACE\" src \"$default_ip\" onlink"
            if ! grep -Fxq "$local_cmd" /etc/rc.local; then
                echo "$local_cmd" >> /etc/rc.local
                echo "配置已写入 /etc/rc.local 。"
            else
                echo "/etc/rc.local 中已存在相同配置，无需重复写入。"
            fi
        fi
    fi
}

# 7. 主菜单
echo "请选择功能："
echo "1. 添加随机 IPv6 地址"
echo "2. 管理默认出口 IPv6 地址"
echo "3. 一键删除全部添加的IPv6地址"
echo "4. 只保留当前出口默认的IPv6地址 (删除其它全部)"
read -p "请输入选择 (1, 2, 3 或 4): " choice_option

case "$choice_option" in
    1)
        add_random_ipv6
        ;;
    2)
        manage_default_ipv6
        ;;
    3)
        delete_all_ipv6
        ;;
    4)
        delete_except_default_ipv6
        ;;
    *)
        echo "无效的选择。"
        exit 1
        ;;
esac
