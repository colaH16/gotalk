module.exports = {
  // [수정] 별이 하나(*)입니다!
  // 의미: "현재 폴더(./)에 있는 html이나 js 파일만 봐라."
  content: ["./*.{html,js}"], 
  theme: {
    extend: {},
  },
  plugins: [require("daisyui")],
  daisyui: {
    themes: ["pastel"],
  },
}