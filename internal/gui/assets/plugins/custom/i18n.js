// The Custom page's words in each language, the English the key: the
// kernel adds them to i18n.js's (magpie.strings). A word i18n.js already
// says is left to it.
magpie.strings({
  zh: {
    "WorkBuddy's free models, asked on a schedule": "WorkBuddy 免费模型，按定时请求",
    "Scheduled requests": "定时请求",
    "A window used up is asked again once its reset has come, and counted back in only on a 200. An account that answers is asked every interval below.": "用尽的窗口到解冻时间后再请求一次，只有返回 200 才算恢复。可用的账号按下面的间隔请求。",
    "Every account that answers": "可用账号的间隔",
    "magpie's own ({n} min)": "magpie 默认（{n} 分钟）",
    "Models watched": "监视的模型",
    "Ask this account and model this often while it answers": "该账号和模型可用时，按这个间隔请求",
    "Minutes between requests, 0 for the setting's own": "请求间隔（分钟），填 0 表示跟随上面的设置",
    "{user}: {n} requests": "{user}：{n} 条请求",
    "Show every account": "显示全部账号",
    "No requests yet. The first one goes when an account's window starts over, or at its interval.": "还没有请求记录。第一条会在账号窗口解冻时，或按间隔发出。",
    "after a window ran out": "解冻后",
    "All requests": "全部请求",
    "Scheduled": "定时",
    "Took": "耗时",
    "Answer": "返回",
    "{from} to now": "{from} 至今",
    "No requests in this window.": "这个窗口内没有请求。",
    "{n} timed": "{n} 次计时",
    "{n} token/s": "{n} token/秒",
    "{i} in · {o} out · {r} cache": "输入 {i} · 输出 {o} · 缓存 {r}",
    "{shown} of {total}": "显示 {shown} / {total}",
  },
});
