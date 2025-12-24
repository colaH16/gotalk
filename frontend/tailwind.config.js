module.exports = {
  // 현재 폴더(frontend) 아래의 모든 html, js 파일을 보겠다는 뜻
  content: ["./**/*.{html,js}"], 
  theme: {
    extend: {},
  },
  plugins: [require("daisyui")],
  daisyui: {
    themes: ["pastel"],
  },
}