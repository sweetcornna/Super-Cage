package io.github.leexeo.cage.mobile;

import android.app.Activity;
import android.content.res.Configuration;
import android.graphics.Typeface;
import android.graphics.drawable.ColorDrawable;
import android.view.View;

// Super Cage palette: ivory paper, dark ink and one clay accent.
final class MobileTheme {
    static final Typeface SERIF = Typeface.create(Typeface.SERIF, Typeface.NORMAL);
    final boolean dark;
    final int bg, panel, ink, muted, line, soft, action, onAction, clay, clayInk, danger, ok;

    private MobileTheme(boolean dark) {
        this.dark = dark;
        bg = dark ? 0xff1f1e1d : 0xfffaf9f5;
        panel = dark ? 0xff2a2926 : 0xffffffff;
        soft = dark ? 0xff34322e : 0xfff0eee6;
        line = dark ? 0xff3d3a35 : 0xffe6e3d8;
        ink = dark ? 0xfff3efe5 : 0xff1f1e1d;
        muted = dark ? 0xffaba597 : 0xff6b655b;
        clay = 0xffd97757;
        clayInk = dark ? 0xffe8936f : 0xffa4482a;
        danger = dark ? 0xfff2b8b5 : 0xffb3261e;
        ok = dark ? 0xff7bc79a : 0xff2e7d4f;
        action = ink;
        onAction = bg;
    }

    static MobileTheme apply(Activity activity, String mode) {
        int night = activity.getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK;
        boolean dark = "dark".equals(mode) || (!"light".equals(mode) && night == Configuration.UI_MODE_NIGHT_YES);
        // The app themes also carry ?attr/cageInk and ?attr/cageGlow for the logo and doodle vectors.
        int style = activity.getResources().getIdentifier(dark ? "CageDark" : "CageLight", "style", activity.getPackageName());
        activity.setTheme(style != 0 ? style : dark ? android.R.style.Theme_Material_NoActionBar : android.R.style.Theme_Material_Light_NoActionBar);
        MobileTheme theme = new MobileTheme(dark);
        activity.getWindow().setBackgroundDrawable(new ColorDrawable(theme.bg));
        activity.getWindow().setStatusBarColor(theme.bg);
        activity.getWindow().setNavigationBarColor(theme.bg);
        activity.getWindow().getDecorView().setSystemUiVisibility(dark ? 0 :
            View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR);
        return theme;
    }
}
