package yomi

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

var (
	readerOnce sync.Once
	reader     *Reader
	readerErr  error
	readerTook time.Duration
)

func testReader(t *testing.T) *Reader {
	t.Helper()
	readerOnce.Do(func() {
		start := time.Now()
		reader, readerErr = New()
		readerTook = time.Since(start)
	})
	if readerErr != nil {
		t.Fatalf("New: %v", readerErr)
	}
	return reader
}

func TestNewIsQuick(t *testing.T) {
	testReader(t)
	if readerTook >= 5*time.Second {
		t.Fatalf("New took %v, want < 5s", readerTook)
	}
}

var castNames = CastNames(
	[]string{"佐藤 美咲", "田中 健", "小林 太郎", "林"},
	[]string{"さとう みさき", "", "こばやし たろう", ""},
)

// kanaCase is one Kana conversion: the name of the case, the input, the cast names (nil for none)
// and the expected kana.
type kanaCase struct {
	name  string
	in    string
	names Names
	want  string
}

func runKana(t *testing.T, tests []kanaCase) {
	t.Helper()
	r := testReader(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Kana(tc.in, tc.names); got != tc.want {
				t.Errorf("Kana(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestKana(t *testing.T) {
	runKana(t, []kanaCase{
		// package doc examples
		{"doc sentence", "火加減を見ながら3分煮ます。", nil, "ひかげんをみながらさんぷんにます。"},
		{"doc 3分", "3分", nil, "さんぷん"},
		{"doc 9月17日", "9月17日", nil, "くがつじゅうしちにち"},
		{"doc 4時", "4時", nil, "よじ"},
		{"doc 28℃", "28℃", nil, "にじゅうはちど"},
		{"doc 2人", "2人", nil, "ふたり"},
		{"doc 3本", "3本", nil, "さんぼん"},
		{"doc 10回", "10回", nil, "じゅっかい"},
		{"doc 28.5度", "28.5度", nil, "にじゅうはってんごど"},
		{"doc 1万人", "1万人", nil, "いちまんにん"},
		{"doc 30%", "30%", nil, "さんじゅっパーセント"},
		{"doc plain number", "28", nil, "にじゅうはち"},
		{"doc non-counter keeps dictionary reading", "2度目の挑戦", nil, "にどめのちょうせん"},
		{"date line", "9月17日、水曜日、朝のニュースです。", nil, "くがつじゅうしちにち、すいようび、あさのニュースです。"},

		// counters: special forms
		{"分", "1分2分3分4分5分6分7分8分9分10分20分100分", nil,
			"いっぷんにふんさんぷんよんぷんごふんろっぷんななふんはっぷんきゅうふんじゅっぷんにじゅっぷんひゃっぷん"},
		{"時", "4時7時9時14時17時19時24時", nil, "よじしちじくじじゅうよじじゅうしちじじゅうくじにじゅうよじ"},
		{"月", "1月4月7月9月10月12月", nil, "いちがつしがつしちがつくがつじゅうがつじゅうにがつ"},
		{"日 1-10 as dates", "9月1日2日3日4日5日6日7日8日9日10日", nil, "くがつついたちふつかみっかよっかいつかむいかなのかようかここのかとおか"},
		{"日 11-31", "11日14日17日18日19日20日21日24日27日29日30日31日", nil,
			"じゅういちにちじゅうよっかじゅうしちにちじゅうはちにちじゅうくにちはつかにじゅういちにちにじゅうよっかにじゅうしちにちにじゅうくにちさんじゅうにちさんじゅういちにち"},
		{"人", "1人2人3人4人7人10人14人100人", nil, "ひとりふたりさんにんよにんしちにんじゅうにんじゅうよにんひゃくにん"},
		{"本", "1本2本3本4本6本8本10本100本1000本1万本", nil,
			"いっぽんにほんさんぼんよんほんろっぽんはっぽんじゅっぽんひゃっぽんせんぼんいちまんぼん"},
		{"杯 匹", "1杯3杯6杯 1匹3匹8匹", nil, "いっぱいさんばいろっぱい いっぴきさんびきはっぴき"},
		{"回 個 軒 箇所", "1回6回8回10回 1個6個 8軒 10箇所 3回", nil, "いっかいろっかいはっかいじゅっかい いっころっこ はっけん じゅっかしょ さんかい"},
		{"階", "1階3階6階", nil, "いっかいさんがいろっかい"},
		{"歳 冊 週", "1歳6歳8歳10歳 1冊8冊 1週10週", nil, "いっさいろくさいはっさいじゅっさい いっさつはっさつ いっしゅうじゅっしゅう"},
		{"度 ℃", "5度 32℃ 0.5度", nil, "ごど さんじゅうにど れいてんごど"},
		{"秒", "1秒30秒", nil, "いちびょうさんじゅうびょう"},
		{"年", "4年 2026年 1年", nil, "よねん にせんにじゅうろくねん いちねん"},
		{"円", "4円 1,500円 12,000円 100円", nil, "よえん せんごひゃくえん いちまんにせんえん ひゃくえん"},
		{"番 枚 台 名 話 位", "3番 5枚 2台 4名 1話 第1位", nil, "さんばん ごまい にだい よんめい いちわ だいいちい"},
		{"曲", "1曲6曲8曲10曲3曲", nil, "いっきょくろっきょくはっきょくじゅっきょくさんきょく"},
		{"点", "1点8点10点100点3点", nil, "いってんはってんじゅってんひゃくてんさんてん"},
		{"percent", "1% 8% 10% 100% 40％ 6パーセント", nil, "いっパーセント はっパーセント じゅっパーセント ひゃくパーセント よんじゅっパーセント ろくパーセント"},
		{"metric", "12.5km 3kg 10cm 5m", nil, "じゅうにてんごキロメートル さんキログラム じゅっセンチメートル ごメートル"},
		{"割 倍", "3割 1倍 2倍", nil, "さんわり いちばい にばい"},
		{"compound counters", "3時間 7時間 2時半 1週間 3ヶ月 3日間 1日間 1日中 3分間", nil,
			"さんじかん ななじかん にじはん いっしゅうかん さんかげつ みっかかん いちにちかん いちにちじゅう さんぷんかん"},
		{"つ", "1つ2つ3つ8つ10つ", nil, "ひとつふたつみっつやっつとお"},

		// numbers
		{"decimals", "3.14 1.5倍 0.5秒 10.0 8.25", nil, "さんてんいちよん いってんごばい れいてんごびょう じゅってんれい はってんにご"},
		{"kanji numerals", "三十分 一万人 百人 千円 二千二十六年 二〇二六年 一千万円 一億円", nil,
			"さんじゅっぷん いちまんにん ひゃくにん せんえん にせんにじゅうろくねん にせんにじゅうろくねん いっせんまんえん いちおくえん"},
		{"hundreds thousands", "300 600 800 1000 3000 8000 10000 0 1兆", nil,
			"さんびゃく ろっぴゃく はっぴゃく せん さんぜん はっせん いちまん れい いっちょう"},
		{"full-width digits", "２０２６年９月１７日、３．５度", nil, "にせんにじゅうろくねんくがつじゅうしちにち、さんてんごど"},
		{"overflow is read digit by digit", "12345678901234567890円", nil,
			"いちにさんよんごろくななはちきゅうぜろいちにさんよんごろくななはちきゅうぜろえん"},

		// what is left alone
		{"loanwords and ASCII", "カメラでABCの映像を3本撮った。", nil, "カメラでABCのえいぞうをさんぼんとった。"},
		{"particles", "私は駅へ荷物を運ぶ。", nil, "わたしはえきへにもつをはこぶ。"},
		{"no kanji unchanged", "こんにちは、テレビ！ OK?", nil, "こんにちは、テレビ！ OK?"},
		{"empty", "", nil, ""},
		{"whitespace kept", "今日は。\n晴れ　です。", nil, "きょうは。\nはれ　です。"},
		{"unknown kanji keeps surface", "龘を見た", nil, "龘をみた"},

		// names
		{"cast names read as given", "佐藤 美咲さんがカメラを持って来ました。", castNames, "さとう みさきさんがカメラをもってきました。"},
		// a given name the dictionary splits into single kanji (悠|真) still matches as one name part
		{"name spanning several tokens, no space", "高橋悠真です。", CastNames([]string{"高橋 悠真"}, []string{"たかはし ゆうま"}), "たかはしゆうまです。"},
		{"cast name without kana stays kanji", "田中さんと健さんです。", castNames, "田中さんと健さんです。"},
		{"longest name first", "小林さんと林さんと小林太郎さん。", castNames, "こばやしさんと林さんとこばやしたろうさん。"},
		{"dictionary reads the same name without cast", "田中さんと佐藤さんです。", nil, "たなかさんとさとうさんです。"},
	})
}

// TestKanaNumbers covers zero, big units, fractions, juxtaposed kanji digits, lists and ranges.
func TestKanaNumbers(t *testing.T) {
	runKana(t, []kanaCase{
		{"zero is れい", "午前0時に警報が解除されました。", nil, "ごぜんれいじにけいほうがかいじょされました。"},
		{"zero percent and decimals", "降水確率は0%、0.5度の差、0度、0.3%", nil, "こうすいかくりつはれいパーセント、れいてんごどのさ、れいど、れいてんさんパーセント"},
		{"零 and 〇", "零時に出発します。気温は0度、〇点、0.5秒、0.05", nil, "れいじにしゅっぱつします。きおんはれいど、れいてん、れいてんごびょう、れいてんれいご"},
		{"〇〇 as a blank is まる", "〇〇さんが来ました。〇〇県", nil, "まるまるさんがきました。まるまるけん"},
		{"decimal before 万億兆", "被害額は3.5億円、1.5万人、2.5万円、1.2億円、3.5兆円", nil, "ひがいがくはさんてんごおくえん、いってんごまんにん、にてんごまんえん、いってんにおくえん、さんてんごちょうえん"},
		{"decimal big unit keeps the counter change", "1.5万本", nil, "いってんごまんぼん"},
		{"bare big unit after 数 何", "数万人が集まった。数十人、数百人、十数人、何万人、数万本、億万長者", nil,
			"すうまんにんがあつまった。すうじゅうにん、すうひゃくにん、じゅうすうにん、なんまんにん、すうまんぼん、おくまんちょうじゃ"},
		{"juxtaposed kanji digits", "七五三のお祝い。二三日、四五人、五六回", nil, "しちごさんのおいわい。にさんにち、しごにん、ごろっかい"},
		{"kanji pair with comma", "二、三日", nil, "に、さんにち"},
		{"kanji year stays positional", "一九九五年、二〇二六年", nil, "せんきゅうひゃくきゅうじゅうごねん、にせんにじゅうろくねん"},
		{"list shares the counter", "7、8日は荒れた天気。1、2日は晴れます", nil, "なのか、ようかはあれたてんき。ついたち、ふつかははれます"},
		{"range reads から", "3～5人でどうぞ。10～20%、3〜4日", nil, "さんにんからごにんでどうぞ。じゅっパーセントからにじゅっパーセント、みっかからよっか"},
		{"fractions", "小さじ1/2の砂糖、塩小さじ1/4、大根1/2本、人参1/3本、3分の1量", nil,
			"こさじにぶんのいちのさとう、しおこさじよんぶんのいち、だいこんにぶんのいっぽん、にんじんさんぶんのいっぽん、さんぶんのいちりょう"},
		{"slash date is not a fraction", "9/17", nil, "きゅう/じゅうなな"},
		{"いっせん after 万", "1万1千円です。1万1000円、一千円、1000万、1000億円、1,500万円", nil,
			"いちまんいっせんえんです。いちまんいっせんえん、いっせんえん、いっせんまん、いっせんおくえん、いっせんごひゃくまんえん"},
		{"large numbers do not share a counter", "1000億、1,500万円", nil, "いっせんおく、いっせんごひゃくまんえん"},
		{"bare 千 stays せん", "千五百万円、数千万円、何千万円、千円、三千円", nil, "せんごひゃくまんえん、すうせんまんえん、なんせんまんえん、せんえん、さんぜんえん"},
		{"english ordinals", "1stアルバム、2ndシングル、3rd", nil, "ファーストアルバム、セカンドシングル、サード"},
		{"digits glued to letters stay", "3D映像と4K放送と2LDK", nil, "3Dえいぞうと4Kほうそうと2LDK"},
	})
}

// TestKanaCounters covers the counter table additions and sound-change fixes.
func TestKanaCounters(t *testing.T) {
	runKana(t, []kanaCase{
		// 1日: calendar date or length of time
		{"1日 as duration", "1日1回、情報を更新します。1日目、1日あたり、1日おきに、1日の感染者数", nil,
			"いちにちいっかい、じょうほうをこうしんします。いちにちめ、いちにちあたり、いちにちおきに、いちにちのかんせんしゃすう"},
		{"1日 before numerals", "1日に2回、1日3食、1日平均、1日3回飲む。1日1万歩", nil, "いちにちににかい、いちにちさんしょく、いちにちへいきん、いちにちさんかいのむ。いちにちいちまんぽ"},
		{"1日 before 前 後 verb", "1日前、1日後、1日寝かせる", nil, "いちにちまえ、いちにちご、いちにちねかせる"},
		{"一日 in kanji", "一日、休みます。一日おき、一日一回、一日目、一日三回、一日中", nil, "いちにち、やすみます。いちにちおき、いちにちいっかい、いちにちめ、いちにちさんかい、いちにちじゅう"},
		{"1日 after a month", "9月1日、今月1日、来月1日に", nil, "くがつついたち、こんげつついたち、らいげつついたちに"},
		{"1日 in a date line", "1日午前0時、1日夜、1日、東京で。1日から3日まで。11月1日は文化の日です。", nil,
			"ついたちごぜんれいじ、ついたちよる、ついたち、とうきょうで。ついたちからみっかまで。じゅういちがつついたちはぶんかのひです。"},
		{"1日 alone", "1日", nil, "ついたち"},
		{"other days unchanged", "2日目、10日目、3日後、3日前、3日中", nil, "ふつかめ、とおかめ、みっかご、みっかまえ、みっかちゅう"},

		// native ひと/ふた counters
		{"native counters", "一晩寝かせます。塩一つまみ。一切れ、一房、一束、一袋、一缶、一箱、一玉。一皿、一振り、一鍋。キャベツ一株。一口", nil,
			"ひとばんねかせます。しおひとつまみ。ひときれ、ひとふさ、ひとたば、ひとふくろ、ひとかん、ひとはこ、ひとたま。ひとさら、ひとふり、ひとなべ。キャベツひとかぶ。ひとくち"},
		{"native counters 2", "一煮立ち、一混ぜ、一手間、生姜一片、一粒、一握り、1つまみ、一口食べる、10口、一切れ、二切れ、一組、二組", nil,
			"ひとにたち、ひとまぜ、ひとてま、しょうがひとかけ、ひとつぶ、ひとにぎり、ひとつまみ、ひとくちたべる、じゅっくち、ひときれ、ふたきれ、ひとくみ、ふたくみ"},
		{"dictionary words with 一 stay", "一口大、一息、一時、一部、一片の雲", nil, "ひとくちだい、ひといき、いちじ、いちぶ、いっぺんのくも"},
		{"組", "4人組のバンド。男性4人組、3人組、5人組、2人組、10組、1組目は、2組目は。2組のデュオ", nil,
			"よにんぐみのバンド。だんせいよにんぐみ、さんにんぐみ、ごにんぐみ、ふたりぐみ、じゅっくみ、ひとくみめは、ふたくみめは。ふたくみのデュオ"},
		{"組 as a class after 年", "3年2組、1年1組", nil, "さんねんにくみ、いちねんいちくみ"},
		{"人 compounds", "1人暮らしの高齢者。3人分、4人分、1人分、3人前、1人前、2階建て、100円玉", nil,
			"ひとりぐらしのこうれいしゃ。さんにんぶん、よにんぶん、ひとりぶん、さんにんまえ、いちにんまえ、にかいだて、ひゃくえんだま"},

		// sound changes
		{"周年", "デビュー10周年、1周年、8周年、20周年、30周年", nil, "デビューじゅっしゅうねん、いっしゅうねん、はっしゅうねん、にじゅっしゅうねん、さんじゅっしゅうねん"},
		{"震度", "最大震度5弱、震度6強の揺れ。6強、6弱、5強、5弱", nil, "さいだいしんどごじゃく、しんどろっきょうのゆれ。ろっきょう、ろくじゃく、ごきょう、ごじゃく"},
		{"センチ キロ", "1cm幅、8cmほど、1センチ、10センチ、60キロ、100キロ、1キロ走る、8キロ、10キロ", nil,
			"いっセンチメートルはば、はっセンチメートルほど、いっセンチ、じゅっセンチ、ろくじゅっキロ、ひゃっキロ、いちキロはしる、はちキロ、じゅっキロ"},
		{"kilo units", "10km、10kg、350キロカロリー、3kcal", nil, "じゅっキロメートル、じゅっキログラム、さんびゃくごじゅっキロカロリー、さんキロカロリー"},
		{"p-row after 千 万", "花火を1万発、3千発、1万歩、1000歩", nil, "はなびをいちまんぱつ、さんぜんぱつ、いちまんぽ、せんぽ"},
		{"はたち", "二十歳になった。20歳", nil, "はたちになった。はたち"},
		{"t-row and か国", "牛が1頭、10頭。1通、10通、1丁、10丁の豆腐、10か国、3か国", nil, "うしがいっとう、じゅっとう。いっつう、じゅっつう、いっちょう、じゅっちょうのとうふ、じゅっかこく、さんかこく"},
		{"軒", "3軒隣。1軒、8軒、10軒", nil, "さんげんとなり。いっけん、はっけん、じゅっけん"},
		{"7人", "7人が来た。7人家族", nil, "しちにんがきた。しちにんかぞく"},
		{"小節 首", "一小節、4小節、百人一首", nil, "いっしょうせつ、よんしょうせつ、ひゃくにんいっしゅ"},
		{"割方", "8割方、7、8割方", nil, "はちわりがた、ななわりがた、はちわりがた"},

		// units
		{"kitchen units", "卵3個と牛乳100mlを混ぜます。水200cc、だし汁300ml、1L、600Wで2分", nil,
			"たまごさんことぎゅうにゅうひゃくミリリットルをまぜます。みずにひゃくシーシー、だしじるさんびゃくミリリットル、いちリットル、ろっぴゃくワットでにふん"},
		{"more units", "1.5L、50cc、500mg、180°C、20km/h", nil, "いってんごリットル、ごじゅっシーシー、ごひゃくミリグラム、ひゃくはちじゅうど、にじゅっキロメートルまいじ"},

		// 何 + counter
		{"何 + counter", "何分かかりますか。何時何分ですか。何時、何人、何日、何本、何階、何泊、何度も、何月何日", nil,
			"なんぷんかかりますか。なんじなんぷんですか。なんじ、なんにん、なんにち、なんぼん、なんがい、なんぱく、なんども、なんがつなんにち"},
		{"何 alone", "何か。何でも。何色", nil, "なにか。なにでも。なにいろ"},

		// a counter glued to a kitchen noun
		{"glued counter and noun", "米2合を研いで、30分水に浸します。3分水に、10分火にかけ、5分油で、1時間油で揚げます", nil,
			"こめにごうをといで、さんじゅっぷんみずにひたします。さんぷんみずに、じゅっぷんひにかけ、ごふんあぶらで、いちじかんあぶらであげます"},
		{"noun after a counter reads on its own", "昆布を一晩水につけて、5分塩を振って", nil, "こんぶをひとばんみずにつけて、ごふんしおをふって"},

		// weekday in brackets
		{"bracketed weekday", "9月17日(水)、18日（木）", nil, "くがつじゅうしちにち(すい)、じゅうはちにち（もく）"},
	})
}

// TestKanaWeather covers forecast and disaster wording.
func TestKanaWeather(t *testing.T) {
	runKana(t, []kanaCase{
		{"降ります", "雨が降りました。雨が降ります。雪が降りやすい状態です。3日間で200ミリの雨が降りました。", nil,
			"あめがふりました。あめがふります。ゆきがふりやすいじょうたいです。みっかかんでにひゃくミリのあめがふりました。"},
		{"降る forms", "雨が降って、雨が降った。雨が降り続く。雨が降り出す。降り始め。明日は雨が降りやすく", nil,
			"あめがふって、あめがふった。あめがふりつづく。あめがふりだす。ふりはじめ。あしたはあめがふりやすく"},
		{"降りる stays おりる", "バスを降りました。電車から降りて。", nil, "バスをおりました。でんしゃからおりて。"},
		{"一時雨 日中", "夕方から一時雨です。日中は真夏日となる所が多いでしょう。", nil, "ゆうがたからいちじあめです。にっちゅうはまなつびとなるところがおおいでしょう。"},
		{"後 is のち", "晴れ後曇り。曇り後雨。風は南の風、後北の風。雨後雪、雨後の筍", nil, "はれのちくもり。くもりのちあめ。かぜはみなみのかぜ、のちきたのかぜ。あめのちゆき、うごのたけのこ"},
		{"後 elsewhere", "3日後に発表します。その後、午後3時から。後で、後ほど、最後", nil, "みっかごにはっぴょうします。そのご、ごごさんじから。あとで、のちほど、さいご"},
		{"寒気", "上空に寒気が流れ込みます。寒気が入る。寒気がする。寒気がしました", nil, "じょうくうにかんきがながれこみます。かんきがはいる。さむけがする。さむけがしました"},
		{"vocabulary", "高潮、平野部、避難所、行楽日和、数日間、数日後、雹、霰、明後日、線状降水帯", nil,
			"たかしお、へいやぶ、ひなんじょ、こうらくびより、すうじつかん、すうじつご、ひょう、あられ、あさって、せんじょうこうすいたい"},
		{"already right", "曇り時々雨、所により、晴れのち曇り、真夏日、猛暑日、熱中症、氷点下、降水確率、最大瞬間風速、土砂崩れ、日本海側、太平洋側、関東甲信、大雨特別警報、梅雨入り、梅雨明け、竜巻、突風、床上浸水", nil,
			"くもりときどきあめ、ところにより、はれのちくもり、まなつび、もうしょび、ねっちゅうしょう、ひょうてんか、こうすいかくりつ、さいだいしゅんかんふうそく、どしゃくずれ、にほんかいがわ、たいへいようがわ、かんとうこうしん、おおあめとくべつけいほう、つゆいり、つゆあけ、たつまき、とっぷう、ゆかうえしんすい"},
		{"中日", "3連休の中日", nil, "さんれんきゅうのなかび"},
	})
}

// TestKanaCooking covers recipe vocabulary.
func TestKanaCooking(t *testing.T) {
	runKana(t, []kanaCase{
		{"鶏", "鶏肉。鶏もも肉1枚、鶏むね肉2枚、鶏手羽先6本。鶏ひき肉、鶏の唐揚げ、鶏のささみ", nil,
			"とりにく。とりももにくいちまい、とりむねにくにまい、とりてばさきろっぽん。とりひきにく、とりのからあげ、とりのささみ"},
		{"鶏 the animal", "鶏が鳴く。鶏が3羽。", nil, "にわとりがなく。にわとりがさんわ。"},
		{"牛 豚", "豚肉と牛肉。牛薄切り肉、牛丼と豚丼と親子丼、牛が3頭、牛乳", nil, "ぶたにくとぎゅうにく。ぎゅううすぎりにく、ぎゅうどんとぶたどんとおやこどん、うしがさんとう、ぎゅうにゅう"},
		{"katakana kept in compounds", "生クリームと生姜を用意します。缶ビール、生ビール、紙コップ、生ハム、大トロ、小エビ。", nil,
			"なまクリームとしょうがをよういします。かんビール、なまビール、かみコップ、なまハム、おおトロ、こエビ。"},
		{"katakana kept 2", "カレー店とテレビ局とガラス窓。バス停、ゴミ箱、ビール瓶、レモン水、オリーブ油、レモン色", nil,
			"カレーてんとテレビきょくとガラスまど。バスてい、ゴミばこ、ビールびん、レモンすい、オリーブゆ、レモンいろ"},
		{"vocabulary", "焼き色がつくまで。長ねぎ、炒め物、落とし蓋、蓮根、茄子、半月切り、細切り、小口切り、汁気、湯切り、唐揚げ", nil,
			"やきいろがつくまで。ながねぎ、いためもの、おとしぶた、れんこん、なす、はんげつぎり、ほそぎり、こぐちぎり、しるけ、ゆきり、からあげ"},
		{"予熱 leaks no kanji", "予熱したオーブンで。オーブンを180℃に予熱して。オーブンを180度に予熱。", nil,
			"よねつしたオーブンで。オーブンをひゃくはちじゅうどによねつして。オーブンをひゃくはちじゅうどによねつ。"},
		{"辛い", "少し辛いので。少し辛いけど美味しい。辛くないですか。辛さ。激辛、辛口、辛味、甘辛い、中辛", nil,
			"すこしからいので。すこしからいけどおいしい。からくないですか。からさ。げきから、からくち、からみ、あまからい、ちゅうから"},
		{"辛い as hardship", "辛い思いをした。辛い経験", nil, "つらいおもいをした。つらいけいけん"},
		{"の間", "弱火から中火の間で", nil, "よわびからちゅうびのあいだで"},
		{"suffix readings", "雪解け水、打ち水、炭酸水、揚げ物、食べ物、茶色、きつね色、ごま油、サラダ油", nil,
			"ゆきどけみず、うちみず、たんさんすい、あげもの、たべもの、ちゃいろ、きつねいろ、ごまあぶら、サラダゆ"},
		{"already right", "火加減、弱火、中火、強火、とろ火、火を通す、火から下ろす、粗熱、水気、一口大、大さじ、小さじ、ひと煮立ち", nil,
			"ひかげん、よわび、ちゅうび、つよび、とろび、ひをとおす、ひからおろす、あらねつ、みずけ、ひとくちだい、おおさじ、こさじ、ひとにたち"},
	})
}

// TestKanaTravel covers travel-narration nouns.
func TestKanaTravel(t *testing.T) {
	runKana(t, []kanaCase{
		{"vocabulary", "港町、干物の匂い、海鮮丼、山道を登る、一本道、秘湯、花畑、内海、外海、門前町、貸切風呂、瀬、河原、手水舎、十人十色、四つ葉", nil,
			"みなとまち、ひもののにおい、かいせんどん、やまみちをのぼる、いっぽんみち、ひとう、はなばたけ、うちうみ、そとうみ、もんぜんまち、かしきりぶろ、せ、かわら、ちょうずや、じゅうにんといろ、よつば"},
		{"rendaku", "岩風呂、家族風呂、お風呂、鉄火丼、絶好の日和", nil, "いわぶろ、かぞくぶろ、おふろ、てっかどん、ぜっこうのひより"},
		{"人気", "人気のない通り。人気の店、人気がない", nil, "ひとけのないとおり。にんきのみせ、にんきがない"},
		{"already right", "木立、干潟、山門、灯り、川下、港、灯台、渓流、木漏れ日、新緑、湖面、岬、峠、棚田、石畳、湯煙、湯船、老舗、女将、仲居、境内、鳥居、五重塔、城下町、宿場町、参道、川床、湖畔、畦道", nil,
			"こだち、ひがた、さんもん、あかり、かわしも、みなと、とうだい、けいりゅう、こもれび、しんりょく、こめん、みさき、とうげ、たなだ、いしだたみ、ゆけむり、ゆぶね、しにせ、おかみ、なかい、けいだい、とりい、ごじゅうのとう、じょうかまち、しゅくばまち、さんどう、かわどこ、こはん、あぜみち"},
		{"left to the dictionary", "明日の朝は。日本人初。十八番。水面。市場", nil, "あしたのあさは。にっぽんじんはつ。じゅうはちばん。すいめん。しじょう"},
	})
}

// TestKanaNames checks that cast names are matched on word boundaries: a name that is also an
// ordinary morpheme leaves the words containing it alone.
func TestKanaNames(t *testing.T) {
	cast := CastNames(
		[]string{"東", "京", "健", "学", "明", "幸", "光", "北", "原", "谷", "森", "林", "愛", "花", "洋", "大西", "恵"},
		[]string{"あずま", "みやこ", "", "まなぶ", "あきら", "さち", "ひかる", "きた", "はら", "たに", "もり", "はやし", "あい", "はな", "ひろし", "おおにし", "めぐみ"},
	)
	runKana(t, []kanaCase{
		{"東京 with cast 東 京", "東京と京都。", cast, "とうきょうときょうと。"},
		{"protected name inside a word", "健全な健康。健康診断", cast, "けんぜんなけんこう。けんこうしんだん"},
		{"words containing one-kanji names", "学校、明日、説明、幸せ、観光、北海道、原因、渋谷、森林、恋愛、花火、太平洋、大西洋、恵まれた", cast,
			"がっこう、あした、せつめい、しあわせ、かんこう、ほっかいどう、げんいん、しぶや、しんりん、れんあい、はなび、たいへいよう、たいせいよう、めぐまれた"},
		{"words containing 林", "林檎、林業、竹林", cast, "りんご、りんぎょう、ちくりん"},
		{"names as words are read", "東さんと京さんと健さんと林さん。大西さんと恵さん", cast, "あずまさんとみやこさんと健さんとはやしさん。おおにしさんとめぐみさん"},
		{"name is not a counter", "健さんが3人。", cast, "健さんがさんにん。"},
	})
}

func TestDialogue(t *testing.T) {
	r := testReader(t)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"two tags, English around",
			"integrated_multimodal_description: A kitchen at dusk. <d>[Japanese] 火加減を見ながら3分煮ます。</d> She turns to the camera. <d>[Japanese] 佐藤 美咲です。今日は9月17日です。</d> overall_soundscape: quiet.",
			"integrated_multimodal_description: A kitchen at dusk. <d>[Japanese] ひかげんをみながらさんぷんにます。</d> She turns to the camera. <d>[Japanese] さとう みさきです。きょうはくがつじゅうしちにちです。</d> overall_soundscape: quiet.",
		},
		{
			"tag whitespace kept verbatim",
			"<d>[Japanese]\n  火加減\n</d>",
			"<d>[Japanese]\n  ひかげん\n</d>",
		},
		{
			"no tag, nothing changes",
			"A man reads 火加減 aloud. 3分",
			"A man reads 火加減 aloud. 3分",
		},
		{
			"other language tag untouched",
			"<d>[English] 3分</d><d>[Japanese] 3分</d>",
			"<d>[English] 3分</d><d>[Japanese] さんぷん</d>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Dialogue(tc.in, castNames); got != tc.want {
				t.Errorf("Dialogue(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCastNames(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		kanas []string
		want  Names
	}{
		{"two parts", []string{"佐藤 美咲"}, []string{"さとう みさき"}, Names{"佐藤": "さとう", "美咲": "みさき"}},
		{"ideographic space", []string{"佐藤　美咲"}, []string{"さとう　みさき"}, Names{"佐藤": "さとう", "美咲": "みさき"}},
		{"one part", []string{"佐藤美咲"}, []string{"さとうみさき"}, Names{"佐藤美咲": "さとうみさき"}},
		{"no kana protects", []string{"田中 健"}, []string{""}, Names{"田中": "", "健": ""}},
		{"missing kana protects", []string{"田中 健"}, nil, Names{"田中": "", "健": ""}},
		{"part count mismatch protects", []string{"田中 健"}, []string{"たなかけん"}, Names{"田中": "", "健": ""}},
		{"reading wins over protection", []string{"田中 健", "田中 花"}, []string{"", "たなか はな"}, Names{"田中": "たなか", "健": "", "花": "はな"}},
		{"empty name skipped", []string{"", "  "}, []string{"a", "b"}, Names{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CastNames(tc.names, tc.kanas); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CastNames(%q, %q) = %v, want %v", tc.names, tc.kanas, got, tc.want)
			}
		})
	}
}

func TestPlaceholderRoundTrip(t *testing.T) {
	r := testReader(t)
	// A private-use rune in the text passes through untouched next to a protected name.
	in := "田中さん"
	if got, want := r.Kana(in, Names{"田中": ""}), in; got != want {
		t.Errorf("Kana(%q) = %q, want %q", in, got, want)
	}
}

func TestHiragana(t *testing.T) {
	for in, want := range map[string]string{
		"ヒカゲン":   "ひかげん",
		"ニュース":   "にゅーす",
		"トウキョウ":  "とうきょう",
		"ヴァイオリン": "ゔぁいおりん",
		"ー":      "ー",
	} {
		if got := hiragana(in); got != want {
			t.Errorf("hiragana(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadingKana(t *testing.T) {
	for _, tc := range []struct{ surface, reading, want string }{
		{"生クリーム", "ナマクリーム", "なまクリーム"},
		{"テレビ局", "テレビキョク", "テレビきょく"},
		{"バス停", "バステイ", "バスてい"},
		{"小エビ", "コエビ", "こエビ"},
		{"火加減", "ヒカゲン", "ひかげん"},
		{"ヴ楽器", "ブガッキ", "ぶがっき"}, // katakana of the surface not in the reading: converted as usual
	} {
		if got := readingKana(tc.surface, tc.reading); got != tc.want {
			t.Errorf("readingKana(%q, %q) = %q, want %q", tc.surface, tc.reading, got, tc.want)
		}
	}
}
