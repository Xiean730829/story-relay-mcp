package main

const defaultCallStoryTitle = "森林冒险"

type callStoryScenario struct {
	Title   string
	Aliases []string
	Opening string
	Beats   []string
}

var callStoryScenarios = []callStoryScenario{
	{
		Title:   defaultCallStoryTitle,
		Aliases: []string{"森林", "森林探险", "兔子", "兔子和胡萝卜", "兔子的胡萝卜"},
		Opening: "雨停了，小兔子和小熊在森林的老树洞里发现一张湿漉漉的地图，箭头指向溪边的旧木桥。",
		Beats: []string{
			"地图边上有一串泥脚印，伙伴可以猜脚印是谁留下的。",
			"旧木桥缺了一块木板，伙伴可以想办法过河。",
			"河对岸有小兔子的胡萝卜棚，桌上可能有胡萝卜蛋糕或下一条线索。",
			"有人在林子里点亮了一盏灯，伙伴可以顺着灯光找回家。",
		},
	},
	{
		Title:   "双熊护林队",
		Aliases: []string{"熊出没", "熊兄弟", "护林队"},
		Opening: "大熊和小熊刚挂好护林队的铃铛，林子里就吹来一顶会走路的帽子。",
		Beats:   []string{"跟着帽子找到走丢的小山羊。", "和修路的邻居商量给小动物留一条路。", "一起把迷路铃挂到大家都听得见的地方。"},
	},
	{
		Title:   "光之巨人守护城",
		Aliases: []string{"奥特曼", "光之巨人", "守护城"},
		Opening: "灯塔城的路灯忽然全灭，一名能借星光变大的小小守护员在广场听见了求助的铃声。",
		Beats:   []string{"先把困在车站的人送到安全的地方。", "发现挡住灯塔的云兽只是找不到回家的路。", "用镜片把星光送回灯塔，也给云兽照路。"},
	},
	{
		Title:   "海底探险",
		Aliases: []string{"海底飞船", "海底小邮局"},
		Opening: "小章鱼在海底邮局收到一封没有地址的信，信封里掉出一枚会吐泡泡的指南针。",
		Beats:   []string{"指南针指向珊瑚礁里的旧船。", "请海龟帮忙绕开太急的水流。", "找到等信的人，猜猜是谁寄的信。"},
	},
	{
		Title:   "太空旅行",
		Aliases: []string{"太空探险", "月球信号"},
		Opening: "小飞船刚落到月球，一个只有茶杯大的机器人就举着纸牌，问谁能帮它找那颗走丢的星星。",
		Beats:   []string{"顺着会闪光的月尘找线索。", "用一块镜子和远处的伙伴发信号。", "把小星星送回能看见它的地方。"},
	},
	{
		Title:   "恐龙谷寻蛋",
		Aliases: []string{"恐龙谷", "恐龙蛋"},
		Opening: "恐龙谷马上要下雨了，两只小恐龙却在山坡上发现一颗正往下滚的斑点蛋。",
		Beats:   []string{"用树枝和大叶子挡住滚动的蛋。", "顺着不同大小的脚印寻找它的家。", "听见蛋壳里轻轻敲了两下。"},
	},
	{
		Title:   "魔法图书馆",
		Aliases: []string{"魔法书", "图书馆"},
		Opening: "图书馆快打烊了，一本绿色的书突然打了个喷嚏，把最后一页吹出了窗户。",
		Beats:   []string{"在会自己移动的书架间找线索。", "问每本书它记得最后一页上的哪句话。", "和伙伴写下新的结尾，帮书回到架上。"},
	},
	{
		Title:   "云上列车",
		Aliases: []string{"云朵列车", "彩虹火车"},
		Opening: "一辆蓝色小火车停在窗边，列车员说它会开到云上，但第一站的站牌被风吹跑了。",
		Beats:   []string{"看云朵形状猜出第一站的名字。", "帮带着大行李的旅客上车。", "大家动手做一块风吹不走的新站牌。"},
	},
	{
		Title:   "雪夜救援队",
		Aliases: []string{"雪地救援", "雪山救援"},
		Opening: "大雪盖住了回村的小路，救援队在窗边看到山坡上有一盏小红灯，一闪一闪的。",
		Beats:   []string{"从安全的小路靠近红灯。", "发现小鹿和装着食物的雪橇陷在雪里。", "给小鹿披上毯子，一起沿路标回村。"},
	},
	{
		Title:   "机器人工坊",
		Aliases: []string{"机器人工厂", "修机器人"},
		Opening: "机器人工坊刚开门，一个小机器人抱着一只轮子进来，说它想参加今天的接力赛。",
		Beats:   []string{"试着给小机器人选一个合适的轮子。", "在工坊里设计一段小小的试跑路线。", "和别的机器人一起完成接力。"},
	},
}

var callStoryByTitle = func() map[string]*callStoryScenario {
	byTitle := make(map[string]*callStoryScenario)
	for i := range callStoryScenarios {
		scenario := &callStoryScenarios[i]
		byTitle[normalizedCallText(scenario.Title)] = scenario
		for _, alias := range scenario.Aliases {
			byTitle[normalizedCallText(alias)] = scenario
		}
	}
	return byTitle
}()

func callStoryTitles() []string {
	titles := make([]string, 0, len(callStoryScenarios))
	for _, scenario := range callStoryScenarios {
		titles = append(titles, scenario.Title)
	}
	return titles
}
