# Y_Cordis

**简体中文** | [English](#english)

> 以 Go 语言重写的 Cordis 内核，作为 DSH 兼容宿主的底座。

[![License: CC BY-NC 4.0](https://img.shields.io/badge/License-CC%20BY--NC%204.0-lightgrey.svg)](./LICENSE)

---

## 这是什么

`Y_Cordis` 是 [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)（DSH）所依赖的 **Cordis 插件内核**的 Go 语言重写。

DSH 的内核由两部分构成，本仓库实现这两部分：

| 上游（TypeScript） | 本仓库（Go） | 规模 |
|---|---|---|
| `vendor/cordis` | `cordis/` | 2696 行 TS |
| `vendor/loader` | `loader/` | 1083 行 TS |
| `vendor/include` | `loader/` 的方言层 | 343 行 TS |

## 设计原则

### 只移植语义，不移植 JS 机制

上游内核大量依赖 JavaScript 的反射能力，这些在 Go 中**没有对应物，也不需要对应物**：

| 上游机制 | 为何不移植 | Go 替代 |
|---|---|---|
| `Proxy` / `Reflect.get(receiver)` | Go 无 Proxy | 显式结构体 + 祖先指针 |
| `withProps` / `createShadow` / `createTraceable` | 属性覆盖与 `this` 重绑定 | 服务方法显式接收 `ctx` 参数 |
| `WeakMap` / `WeakKey` | GC 时机不确定 | 确定性显式清理 |
| `Object.getPrototypeOf` 原型链 | 原型继承 | 结构体嵌入 / `parent` 指针 |
| `composeError` / 拼接异步栈 | JS 异步栈帧 | Go 原生 `error` + `%w` |
| `Symbol.hasInstance` / 可调用对象 | JS 函数双重身份 | 显式方法 |

**保留的是语义契约**：

- `FiberState` 状态机（`PENDING` / `LOADING` / `ACTIVE` / `FAILED` / `DISPOSED` / `UNLOADING`，同序）
- `effect` 可逆效果，**LIFO** 回收；setup 失败不留注册
- `inject` epoch 反应性：依赖服务变化即重算，过期 epoch 不执行插件代码
- `waterfall` 的 `next()` 委托与短路否决
- `isolate`（服务名 → 隔离标签）与 `intercept`（服务名 → 配置）
- 祖先链解析，`strict` 门只认 `ACTIVE` 提供者
- 五种派发：`emit` / `parallel` / `serial` / `bail` / `waterfall`

### 加载器方言按官方 `!!js` 语义

`cordis.patch.yml` 的基础 schema 是 **`yaml.JSON_SCHEMA`**（非默认 schema），扩展一个 `!!js` 标量类型：

```ts
// vendor/include/src/index.ts
const JsExpr = new yaml.Type('tag:yaml.org,2002:js', {
  kind: 'scalar',
  resolve: (data) => typeof data === 'string',
  construct: (data) => ({ __jsExpr: data }),
})
export const entryListSchema = yaml.JSON_SCHEMA.extend(JsExpr)
```

由此推出的可观测行为（均以金标准测试钉住）：

- `yes` / `no` / `on` / `off` 是**字符串**（JSON_SCHEMA 不含 YAML 1.1 bool）
- `2026-01-01` 是**字符串**（不含 timestamp 类型）
- `010` 是十进制 **10**
- `1_000` 是**字符串**
- 映射的**键序是可观测行为**（`sortKeys` / `--dump-config` 依赖插入序）

### 两阶段求值

`!!js` **不在解析期求值**。官方在两个不同时机、对不同作用域求值：

| 字段 | 时机 | 作用域 |
|---|---|---|
| `disabled` | **每次挂载决策**重算 | 装载器上下文 |
| `config` | **该行 inject 就绪后** | 该行自身的 fiber 上下文 |

一次性整树插值是错的：`ctx.webStartup.host` 在解析期必然读到 `undefined`。

## 状态

| 组件 | 内容 | 测试 |
|---|---|---|
| `loader/` | YAML 方言、patch 算法、两阶段求值 seam | 金标准对照 + 105 条对抗语料 |
| `cordis/` | 状态机、effect LIFO、epoch 反应性、五种派发 | 语义测试 + 变异验证 |

验收方法：以官方 `js-yaml` 解析官方 bundle 的结果作为**金标准**，Go 实现逐值 + **逐键序**比对。

## 许可证

[CC BY-NC 4.0](./LICENSE) —— 与 [RP-Hub](https://github.com/STA1N156/RP-Hub) 相同。

**明确禁止任何形式的商业化使用。** 任何使用者必须遵守该协议，尊重原作者的署名权。

## 标准

**永远以 [STA1N156/RP-Hub](https://github.com/STA1N156/RP-Hub) 的官方最新版为唯一标准。**

当前钉版：commit [`3e7c706`](https://github.com/STA1N156/RP-Hub/commit/3e7c7064e2c72085d18e729fb2c0260c1894ae2c)（2026-10-15）。

---

## English

`Y_Cordis` is a Go rewrite of the Cordis plugin kernel that DeepSeek Harness depends on.

**Design principle**: port the SEMANTICS, not the JavaScript machinery. `Proxy`, `WeakMap`, prototype chains, and async-stack splicing have no Go counterpart and are deliberately not ported. What is ported is the contract those mechanisms implement: the fiber state machine, LIFO reversible effects, inject-epoch reactivity, waterfall delegation, isolation and intercept scoping, and five event dispatch modes.

**Loader dialect**: the base schema is `yaml.JSON_SCHEMA` extended with a `!!js` scalar, so `yes`/`no` are strings, `2026-01-01` is a string, and mapping key order is observable.

**Two-phase evaluation**: `!!js` is never evaluated at parse time. `disabled` is re-evaluated against the loader scope at every mount decision; `config` is evaluated against the row's own fiber scope only after its injections activate.

**Standard**: [STA1N156/RP-Hub](https://github.com/STA1N156/RP-Hub) official latest, always. Pinned at commit [`3e7c706`](https://github.com/STA1N156/RP-Hub/commit/3e7c7064e2c72085d18e729fb2c0260c1894ae2c).

**License**: [CC BY-NC 4.0](./LICENSE) — non-commercial use only.
