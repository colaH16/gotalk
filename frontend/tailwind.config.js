module.exports = {
  content: ["./static/**/*.html"], // HTML 파일 위치
  theme: {
    extend: {},
  },
  plugins: [require("daisyui")],
  daisyui: {
    themes: ["pastel"], // 테마 설정
  },
}