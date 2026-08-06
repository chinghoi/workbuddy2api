#!/usr/bin/env python3
"""
本地触发词定位器（TEMP-DEBUG，事后删除）。

流程：
  1. 读取 data/full_input.json（由 handler 的 dumpFullInput 写入的完整 input 数组）。
  2. 把 input 数组包装成 /v1/responses 请求发给本机中继（127.0.0.1:7863）。
  3. 检测上游是否回"敏感内容"拒答，并二分定位触发句（优先 tools[].description）。
"""
import json
import sys
import urllib.request

RELAY = "http://127.0.0.1:7863/v1/responses"
API_KEY = "39c7a22ab1be6f36aca2f1022fc74c08615673d8be21431808d0a691e7a8cc5d"


def call(items, model="glm-5.2", stream=True):
    body = json.dumps({
        "model": model,
        "input": items,
        "stream": stream,
    }).encode()
    req = urllib.request.Request(
        RELAY, data=body, method="POST",
        headers={"Authorization": f"Bearer {API_KEY}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            data = r.read().decode("utf-8", "replace")
            status = r.status
    except urllib.error.HTTPError as e:
        data = e.read().decode("utf-8", "replace")
        status = e.code
    rejected = ("敏感内容" in data) or ("敏感" in data and "无法响应" in data)
    return status, rejected, data


def summarize(status, rejected, data):
    flag = "REJECT(敏感内容)" if rejected else "OK"
    snippet = data.replace("\n", " ")[:200]
    print(f"  -> status={status} {flag} :: {snippet}")


def main():
    try:
        with open("data/full_input.json") as f:
            full = json.load(f)
    except FileNotFoundError:
        print("data/full_input.json 不存在。请先在 ChatGPT.app 发一条消息触发捕获。")
        sys.exit(1)

    print(f"input 数组长度 = {len(full)}")
    dev = full[0] if full and full[0].get("role") == "developer" else None
    print(f"developer 元素: keys={list(dev.keys()) if dev else 'N/A'}")

    print("\n[TEST 0] 完整 input（复现拒答）:")
    s, rj, d = call(full)
    summarize(s, rj, d)
    if not rj:
        print("完整 input 未触发拒答 —— 说明触发句不在已捕获内容中，或已修复。退出。")
        return

    # 触发在 developer 元素内。逐字段剥离定位。
    if dev is None:
        print("没有 developer 元素，无法继续二分。")
        return

    dev_stripped_tools = {k: v for k, v in dev.items() if k != "tools"}
    print("\n[TEST 1] developer 去掉 tools 字段:")
    s, rj, d = call([dev_stripped_tools, {"role": "user", "content": "hi"}])
    summarize(s, rj, d)
    trigger_in_tools = rj

    if trigger_in_tools and "tools" in dev:
        descs = []
        for t in dev["tools"]:
            if "description" in t:
                descs.append(t["description"])
        print(f"\n[TEST 2] developer 仅保留 tools（去掉其它字段），tools 数={len(dev['tools'])}:")
        s, rj, d = call([{"role": "developer", "tools": dev["tools"]}, {"role": "user", "content": "hi"}])
        summarize(s, rj, d)

        # 二分每个 description
        for idx, desc in enumerate(descs):
            print(f"\n--- tools[{idx}] description 二分 (len={len(desc)}) ---")
            bisect_desc(desc, idx)
    else:
        # 触发在 content 或其它字段
        content = dev.get("content")
        if content:
            print("\n[TEST 3] developer 仅保留 content:")
            s, rj, d = call([{"role": "developer", "content": content}, {"role": "user", "content": "hi"}])
            summarize(s, rj, d)


def bisect_desc(desc, idx, depth=0):
    if len(desc) < 40:
        print(f"    [leaf] description[{idx}] 整段疑似触发句 (len={len(desc)}): {desc!r}")
        return
    mid = len(desc) // 2
    left = desc[:mid]
    right = desc[mid:]
    tools_l = [{"description": left}]
    tools_r = [{"description": right}]
    print(f"  depth={depth} 左半 (len={len(left)}):")
    s, rj, d = call([{"role": "developer", "tools": tools_l}, {"role": "user", "content": "hi"}])
    summarize(s, rj, d)
    print(f"  depth={depth} 右半 (len={len(right)}):")
    s2, rj2, d2 = call([{"role": "developer", "tools": tools_r}, {"role": "user", "content": "hi"}])
    summarize(s2, rj2, d2)
    if rj:
        bisect_desc(left, idx, depth + 1)
    elif rj2:
        bisect_desc(right, idx, depth + 1)
    else:
        print("  两半都不触发 —— 触发句可能跨分界或被拆断，需整段替换测试。")


if __name__ == "__main__":
    main()
