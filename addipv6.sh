#!/bin/bash
echo "====================================="
echo "欢迎使用 ADD IPv6 管理工具"
echo "作者: Joey"
echo "博客: joeyblog.net"
echo "TG群: https://t.me/+ft-zI76oovgwNmRh"
echo "提醒: 合理使用"
echo "====================================="

if [ "$(id -u)" -ne 0 ]; then
    echo "请以root权限执行此脚本。"
    exit 1
fi

function choose_interface() {
    GLOBAL_IPV6_INTERFACES=()
    for iface in $(ip -o link show | awk -F': ' '{print $2}'); do
        if ip -6 addr show dev "$iface" scope global | grep -q 'inet6'; then
            GLOBAL_IPV6_INTERFACES+=("$iface")
        fi
    done
    if [ ${#GLOBAL_IPV6_INTERFACES[@]} -eq 0 ]; then
        echo "未检测到具有全局 IPv6 地址的网卡，请检查 VPS 的网络配置。"
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

function add_random_ipv6() {
    choose_interface
    ipv6_cidr=$(ip -6 addr show dev "$SELECTED_IFACE" scope global | awk '/inet6/ {print $2}' | head -n1)
    if [ -z "$ipv6_cidr" ]; then
        echo "无法获取 $SELECTED_IFACE 的全局 IPv6 地址。"
        exit 1
    fi
    BASE_ADDR=$(echo "$ipv6_cidr" | cut -d'/' -f1)
    PLEN=$(echo "$ipv6_cidr" | cut -d'/' -f2)
    echo "检测到IPv6地址: $ipv6_cidr"
    echo "使用的网络: $BASE_ADDR/$PLEN"
    # 如果前缀为 /128，则不支持随机生成其它地址
    if [ "$PLEN" -eq 128 ]; then
         echo "检测到前缀为 /128，不支持随机生成其它地址。"
         exit 1
    fi
    read -p "请输入要添加的随机 IPv6 地址数量: " COUNT
    if ! [[ "$COUNT" =~ ^[0-9]+$ ]]; then
        echo "数量必须为整数。"
        exit 1
    fi
    for (( i=1; i<=COUNT; i++ )); do
         RANDOM_ADDR=$(python3 -c "import ipaddress, random; net=ipaddress.ip_network('$BASE_ADDR/$PLEN', strict=False); print(ipaddress.IPv6Address(random.randint(int(net.network_address), int(net.broadcast_address))))")
         echo "添加地址: ${RANDOM_ADDR}/${PLEN}"
         ip -6 addr add "${RANDOM_ADDR}/${PLEN}" dev "$SELECTED_IFACE"
         echo "${RANDOM_ADDR}/${PLEN}" >> /tmp/added_v6_ipv6.txt
    done
    echo "所有地址添加完成。"
}

function manage_default_ipv6() {
    choose_interface
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
    SELECTED_ENTRY="${ipv6_list[$((addr_choice-1))]}"
    SELECTED_IP=$(echo "$SELECTED_ENTRY" | cut -d'/' -f1)
    echo "选择的默认出口IPv6地址为：$SELECTED_IP"
    GATEWAY=$(ip -6 route show default dev "$SELECTED_IFACE" | awk '/default/ {print $3}' | head -n1)
    if [ -z "$GATEWAY" ]; then
        echo "未检测到 $SELECTED_IFACE 的默认IPv6路由，请确认网络配置。"
        exit 1
    fi
    echo "检测到默认IPv6网关为：$GATEWAY"
    ip -6 route change default via "$GATEWAY" dev "$SELECTED_IFACE" src "$SELECTED_IP"
    if [ $? -eq 0 ]; then
        echo "默认出口IPv6地址更新成功，出站流量将使用 $SELECTED_IP 作为源地址。"
    else
        echo "更新默认出口IPv6地址失败，请检查系统路由配置。"
    fi
    read -p "是否将此配置写入 /etc/rc.local 以避免重启后失效？(y/n): " persist_choice
    if [[ "$persist_choice" =~ ^[Yy]$ ]]; then
        if [ ! -f /etc/rc.local ]; then
            echo "#!/bin/bash" > /etc/rc.local
            chmod +x /etc/rc.local
        fi
        echo "ip -6 route change default via \"$GATEWAY\" dev \"$SELECTED_IFACE\" src \"$SELECTED_IP\"" >> /etc/rc.local
        echo "配置已写入 /etc/rc.local 。"
    fi
}

function delete_all_ipv6() {
    choose_interface
    if [ ! -f /tmp/added_v6_ipv6.txt ]; then
        echo "未检测到已添加的IPv6地址记录文件。"
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

function delete_except_default_ipv6() {
    choose_interface
    default_ip=$(ip -6 route show default dev "$SELECTED_IFACE" | awk '{for(i=1;i<=NF;i++){if($i=="src"){print $(i+1); exit}}}')
    if [ -z "$default_ip" ]; then
        echo "未检测到默认出口IPv6地址，请先设置默认出口IPv6地址。"
        exit 1
    fi
    echo "当前默认出口IPv6地址: $default_ip"
    mapfile -t ip_entries < <(ip -6 addr show dev "$SELECTED_IFACE" scope global | awk '/inet6/ {print $2}')
    for entry in "${ip_entries[@]}"; do
         addr_only=$(echo "$entry" | cut -d'/' -f1)
         if [ "$addr_only" != "$default_ip" ]; then
             echo "删除地址: $entry"
             ip -6 addr del "$entry" dev "$SELECTED_IFACE"
         fi
    done
    echo "保留默认出口IPv6地址 $default_ip，其它IPv6地址已删除。"
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
             echo "ip -6 route change default via \"$gateway\" dev \"$SELECTED_IFACE\" src \"$default_ip\"" >> /etc/rc.local
             echo "配置已写入 /etc/rc.local 。"
         fi
    fi
}

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
