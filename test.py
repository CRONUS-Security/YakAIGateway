import json
import requests


def test_chat_completion():
  # API 配置
  base_url = "http://127.0.0.1:8787"
  endpoint = f"{base_url}/v1/chat/completions"
  api_key = "sk-test"

  # 请求头
  headers = {
      "Content-Type": "application/json",
      "Authorization": f"Bearer {api_key}",
  }

  # 请求体（Payload）
  payload = {
      "model": "memfit-blade-free",  # 可根据你的本地/代理服务实际支持的模型名称修改
      "messages": [
        #   {
        #       "role": "system",
        #       "content": "You are a helpful and concise assistant.",
        #   },
          {
              "role": "user",
              "content": "如实介绍你是什么模型",
          },
      ],
      # "temperature": 0.7,
      "stream": False,  # 如果需要测试流式输出，可以改为 True 并处理 SSE
  }

  print(f"正在向 {endpoint} 发送请求...")

  try:
    # 发送请求（设置超时时间防止长时间无响应）
    response = requests.post(
        endpoint, headers=headers, json=payload, timeout=30
    )

    # 检查响应状态码
    if response.status_code == 200:
      print("\n✅ 请求成功！返回结果如下：\n")
      result = response.json()

      # 打印完整 JSON 结构（可选）
      # print(json.dumps(result, indent=2, ensure_ascii=False))

      # 提取并打印大模型回复的内容
      reply_content = result["choices"][0]["message"]["content"]
      print(f"AI 回复: {reply_content}")

    else:
      print(f"\n❌ 请求失败，状态码: {response.status_code}")
      print(f"错误信息: {response.text}")

  except requests.exceptions.ConnectionError:
    print(f"\n❌ 连接错误：无法连接到 {base_url}，请检查服务是否已启动。")
  except requests.exceptions.Timeout:
    print("\n❌ 请求超时：服务端响应时间过长。")
  except Exception as e:
    print(f"\n❌ 发生未知错误: {e}")


if __name__ == "__main__":
  test_chat_completion()